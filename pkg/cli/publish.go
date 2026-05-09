package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alkk/repokit/pkg/config"
	"github.com/alkk/repokit/pkg/index"
	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
	"github.com/alkk/repokit/pkg/sign"
)

// PublishCmd rebuilds dists/<distro>/<suite>/ for one or more scopes and signs
// the resulting Release. It does not touch pool/ or the packages table.
type PublishCmd struct {
	Distro string `long:"distro" description:"target distribution (omit with --suite to publish every suite known to the database)"`
	Suite  string `long:"suite" description:"target suite"`
	NoSign bool   `long:"no-sign" description:"skip signing — produce stub InRelease/Release.gpg via NopSigner"`
}

// Execute runs the publish subcommand.
func (cmd *PublishCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "publish: %v\n", err)
		return ExitError
	}
	return ExitOK
}

const (
	distsDir           = "dists"
	suiteSuffixNew     = ".new"
	suiteSuffixOld     = ".old"
	metaLastPublishKey = "last_publish_at"
	archAll            = "all"
	releaseFilename    = "Release"
)

// scope is one (distro, suite) target the publish run rebuilds.
type scope struct {
	distro string
	suite  string
}

func (cmd *PublishCmd) run(c *Context) error {
	if err := validateScopeName(fieldDistro, cmd.Distro); err != nil {
		return err
	}
	if err := validateScopeName(fieldSuite, cmd.Suite); err != nil {
		return err
	}

	ctx := context.Background()
	cfg := c.Config

	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	scopes, err := cmd.resolveScopes(ctx, c.PublicDir, r)
	if err != nil {
		return err
	}
	if len(scopes) == 0 {
		if !c.Quiet {
			fmt.Fprintln(c.Stdout, "no suites to publish")
		}
		return nil
	}

	signer, err := pickSigner(cmd.NoSign, cfg.SigningKey)
	if err != nil {
		return err
	}

	for _, sc := range scopes {
		if err := publishScope(ctx, c.PublicDir, r, cfg, signer, sc.distro, sc.suite); err != nil {
			return fmt.Errorf("publishing %s/%s: %w", sc.distro, sc.suite, err)
		}
	}

	if err := setLastPublish(ctx, r, time.Now()); err != nil {
		return err
	}
	if !c.Quiet {
		fmt.Fprintf(c.Stdout, "published %d suite(s)\n", len(scopes))
	}
	return nil
}

// resolveScopes converts the flag combination to the list of (distro, suite)
// targets to rebuild. Both empty fans out to every suite present in the DB
// or as an existing dists/ tree on disk; a single side specified without the
// other is rejected.
func (cmd *PublishCmd) resolveScopes(ctx context.Context, publicDir string, r *repo.Repo) ([]scope, error) {
	if cmd.Distro != "" && cmd.Suite != "" {
		return []scope{{distro: cmd.Distro, suite: cmd.Suite}}, nil
	}
	if cmd.Distro != "" || cmd.Suite != "" {
		return nil, errors.New("--distro and --suite must be provided together (or both omitted to publish all)")
	}
	return allScopes(ctx, publicDir, r)
}

// pickSigner selects the signing backend. --no-sign always picks NopSigner.
// Otherwise GPGSigner is used, requiring a non-empty signing_key in config so
// gpg has a default key to sign with. Whitespace-only keys are treated as
// empty here (rather than at config-load time, where they're tolerated so
// add/delete/verify don't regress when signing_key is unset) — passing a
// whitespace value through to gpg would fail later with a confusing error.
func pickSigner(noSign bool, keyID string) (sign.Signer, error) {
	if noSign {
		return sign.NopSigner{}, nil
	}
	if strings.TrimSpace(keyID) == "" {
		return nil, errors.New("config.signing_key is empty (set it or pass --no-sign)")
	}
	return sign.GPGSigner{KeyID: keyID}, nil
}

// publishScope rebuilds one suite's dists tree, signs Release, and atomically
// promotes the staging directory to live. The whole flow runs under the
// SQLite writer lock so a concurrent `add`/`delete` cannot commit between
// the per-(component, arch) reads (producing indexes with rows from
// different snapshots), and so two concurrent publishes on the same suite
// cannot race the staging directories (one's cleanStaging would otherwise
// `os.RemoveAll` the other's in-flight `.new`).
func publishScope(ctx context.Context, publicDir string, r *repo.Repo, cfg *config.Config,
	signer sign.Signer, distro, suite string) error {
	if err := r.WithWriterLock(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return publishScopeLocked(ctx, publicDir, r, cfg, signer, distro, suite)
	}); err != nil {
		return fmt.Errorf("publish transaction: %w", err)
	}
	return nil
}

// publishScopeLocked is the body of publishScope, running under the writer
// lock. Reads through r.db still see the latest committed state; the lock's
// role is to block other writers (add/delete/verify --gc/concurrent
// publishScope) from committing until the swap completes.
func publishScopeLocked(ctx context.Context, publicDir string, r *repo.Repo, cfg *config.Config,
	signer sign.Signer, distro, suite string) (err error) {
	distsParent := filepath.Join(publicDir, distro, distsDir)
	livePath := filepath.Join(distsParent, suite)
	newPath := livePath + suiteSuffixNew
	oldPath := livePath + suiteSuffixOld

	// cleanStaging runs first so a prior crashed swap (live missing, .old
	// present) is recovered before any preflight check that might abort —
	// otherwise an arch=all-only suite (or any other rejection below) would
	// leave the suite stranded at <suite>.old with no live tree.
	err = cleanStaging(livePath, newPath, oldPath)
	if err != nil {
		return err
	}
	// Install the staging-cleanup defer before any operation that could leave
	// a partial <suite>.new on disk. MkdirAllSync runs os.MkdirAll first and
	// then fsyncs each new parent; if an fsync fails after MkdirAll succeeded
	// the directory is on disk but MkdirAllSync returns an error, so the
	// defer must already be armed to wipe it. RemoveAll on a non-existent
	// path is a no-op, so installing this before MkdirAllSync is safe even
	// when nothing was created. swapDists renames .new → live on the success
	// path, leaving RemoveAll a no-op there too. cleanStaging at the start
	// of the next run is the safety net for crashes where this defer doesn't
	// fire.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(newPath)
		}
	}()

	components, architectures, err := r.EnumerateForSuite(ctx, distro, suite)
	if err != nil {
		return fmt.Errorf("enumerating suite: %w", err)
	}
	concrete := index.ConcreteArches(architectures)
	if len(components) > 0 && len(concrete) == 0 {
		return errors.New(
			"contains only arch=all packages — apt requires at least one concrete-architecture package per suite (arch=all rows are merged into every binary-<arch>/Packages, never published on their own)")
	}

	if err = pool.MkdirAllSync(newPath); err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}

	fileSets, archs, presentComponents, err := writeIndexes(ctx, r, newPath, distro, suite, components, concrete)
	if err != nil {
		return err
	}

	releasePath := filepath.Join(newPath, releaseFilename)
	err = writeReleaseFile(releasePath, releaseMetaFor(cfg, distro, suite, archs, presentComponents), fileSets)
	if err != nil {
		return err
	}

	if err = signer.Sign(ctx, releasePath); err != nil {
		return fmt.Errorf("signing Release: %w", err)
	}
	// fsync every directory in the staged tree so the file entries written
	// above are durable before swapDists makes the tree live. swapDists only
	// fsyncs distsParent (where the rename happens); the staged tree itself
	// would otherwise carry potentially-uncommitted dir entries into live.
	if err = pool.FsyncTreeDirs(newPath); err != nil {
		return fmt.Errorf("syncing staged tree: %w", err)
	}
	return swapDists(distsParent, livePath, newPath, oldPath)
}

// cleanStaging removes leftover .new and .old siblings from a previous
// crashed run. If the live tree is missing but .old exists, it is the
// previous live tree stranded by a swap whose rollback also failed —
// promote it back before any removal so we never blanket-wipe the only
// remaining copy. RemoveAll on a missing path is a no-op.
func cleanStaging(livePath, newPath, oldPath string) error {
	if err := recoverLiveFromOld(livePath, oldPath); err != nil {
		return err
	}
	if err := os.RemoveAll(newPath); err != nil {
		return fmt.Errorf("removing stale %s: %w", newPath, err)
	}
	if err := os.RemoveAll(oldPath); err != nil {
		return fmt.Errorf("removing stale %s: %w", oldPath, err)
	}
	return nil
}

// recoverLiveFromOld promotes <suite>.old back to <suite> when the live tree
// is missing — the surviving artifact of a swap whose live→.old succeeded
// but whose .new→live promote and rollback both failed.
func recoverLiveFromOld(livePath, oldPath string) error {
	_, err := os.Stat(livePath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", livePath, err)
	}
	_, oerr := os.Stat(oldPath)
	if errors.Is(oerr, os.ErrNotExist) {
		return nil
	}
	if oerr != nil {
		return fmt.Errorf("stat %s: %w", oldPath, oerr)
	}
	if rerr := os.Rename(oldPath, livePath); rerr != nil {
		return fmt.Errorf("recovering live tree from %s: %w", oldPath, rerr)
	}
	return nil
}

// writeIndexes walks every (component, concrete-arch) pair present in the
// suite, writes the Packages triple, and accumulates FileSets with paths
// already prefixed by the component/binary-arch directory so they slot
// straight into the Release checksum blocks. Components and architectures in
// the returned slices are limited to those that produced at least one stanza
// — empty (component, arch) pairs are skipped. The caller is responsible for
// enumerating components and for enforcing the arch=all-only guard before
// any staging directory is created; concrete must already be the output of
// index.ConcreteArches (i.e. with "all" filtered out) so the inner loop
// never tries to write a binary-all/ tree.
func writeIndexes(ctx context.Context, r *repo.Repo, newPath, distro, suite string,
	components, concrete []string,
) ([]index.FileSet, []string, []string, error) {
	var files []index.FileSet
	presentArches := map[string]struct{}{}
	presentComponents := map[string]struct{}{}

	for _, component := range components {
		for _, arch := range concrete {
			rows, err := rowsForComponentArch(ctx, r, distro, suite, component, arch)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(rows) == 0 {
				continue
			}
			binaryDir := filepath.Join(newPath, component, "binary-"+arch)
			set, err := index.WritePackagesAll(rows, binaryDir)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("writing %s/binary-%s: %w", component, arch, err)
			}
			prefix := component + "/binary-" + arch + "/"
			set.Plain.Path = prefix + set.Plain.Path
			set.GZ.Path = prefix + set.GZ.Path
			set.XZ.Path = prefix + set.XZ.Path
			files = append(files, set)
			presentArches[arch] = struct{}{}
			presentComponents[component] = struct{}{}
		}
	}

	return files, sortedKeys(presentArches), sortedKeys(presentComponents), nil
}

// rowsForComponentArch returns rows for a (distro, suite, component, arch)
// scope plus any arch=all rows in the same component (which by apt convention
// land in every concrete binary-<arch> Packages). Output is sorted by name
// then version for stable index output.
func rowsForComponentArch(ctx context.Context, r *repo.Repo,
	distro, suite, component, arch string) ([]repo.Package, error) {
	archRows, err := r.List(ctx, repo.Filter{
		Distro: distro, Suite: suite, Component: component, Architecture: arch,
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s rows: %w", arch, err)
	}
	if arch == archAll {
		return archRows, nil
	}
	allRows, err := r.List(ctx, repo.Filter{
		Distro: distro, Suite: suite, Component: component, Architecture: archAll,
	})
	if err != nil {
		return nil, fmt.Errorf("listing arch=all rows: %w", err)
	}
	merged := make([]repo.Package, 0, len(archRows)+len(allRows))
	merged = append(merged, archRows...)
	merged = append(merged, allRows...)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Name != merged[j].Name {
			return merged[i].Name < merged[j].Name
		}
		return merged[i].Version < merged[j].Version
	})
	return merged, nil
}

// sortedKeys returns the keys of set as a sorted slice for deterministic output.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// releaseMetaFor merges per-suite config defaults with the discovered arches
// and components, producing the header values for the Release file.
func releaseMetaFor(cfg *config.Config, distro, suite string, archs, components []string) index.ReleaseMeta {
	sc := cfg.SuiteOrDefault(distro, suite)
	return index.ReleaseMeta{
		Origin:        sc.Origin,
		Label:         sc.Label,
		Suite:         sc.Suite,
		Codename:      sc.Codename,
		Description:   sc.Description,
		Date:          time.Now().UTC(),
		Architectures: archs,
		Components:    components,
	}
}

// writeReleaseFile materializes Release at path, fsyncing the file before
// closing so its bytes are durable before the signer reads them.
func writeReleaseFile(path string, meta index.ReleaseMeta, files []index.FileSet) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if werr := index.WriteRelease(meta, files, f); werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("writing Release: %w", werr)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync Release: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing Release: %w", err)
	}
	return nil
}

// swapDists atomically replaces the live tree with the staged one. The live
// tree is moved aside to .old first so the rename of .new → live is the only
// short-lived inconsistent window. If the .new → live promotion fails after
// the live tree has been demoted, the demotion is reverted: cleanStaging on
// the next publish run wipes .old, so leaving the previous live there would
// permanently lose it.
func swapDists(distsParent, live, fresh, old string) error {
	if err := pool.MkdirAllSync(distsParent); err != nil {
		return fmt.Errorf("creating dists parent: %w", err)
	}
	demoted := false
	if _, err := os.Stat(live); err == nil {
		if rerr := os.Rename(live, old); rerr != nil {
			return fmt.Errorf("moving live aside: %w", rerr)
		}
		demoted = true
		if ferr := pool.FsyncDir(distsParent); ferr != nil {
			return fmt.Errorf("fsync after live→old: %w", ferr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", live, err)
	}
	if err := os.Rename(fresh, live); err != nil {
		if demoted {
			if rbErr := os.Rename(old, live); rbErr != nil {
				return errors.Join(
					fmt.Errorf("promoting new tree: %w", err),
					fmt.Errorf("restoring previous live from %s: %w", old, rbErr),
				)
			}
		}
		return fmt.Errorf("promoting new tree: %w", err)
	}
	if err := pool.FsyncDir(distsParent); err != nil {
		return fmt.Errorf("fsync after new→live: %w", err)
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("removing old tree: %w", err)
	}
	return nil
}

// allScopes returns every distinct (distro, suite) tuple known to the repo —
// both rows present in the packages table and live trees under dists/.
// Including filesystem scopes ensures that suites which were emptied by
// `delete` still get republished (with empty indexes) so apt clients stop
// seeing the stale Packages files.
func allScopes(ctx context.Context, publicDir string, r *repo.Repo) ([]scope, error) {
	seen := map[scope]struct{}{}

	dbScopes, err := dbScopes(ctx, r)
	if err != nil {
		return nil, err
	}
	for _, s := range dbScopes {
		seen[s] = struct{}{}
	}

	fsScopes, err := filesystemScopes(publicDir)
	if err != nil {
		return nil, err
	}
	for _, s := range fsScopes {
		seen[s] = struct{}{}
	}

	out := make([]scope, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].distro != out[j].distro {
			return out[i].distro < out[j].distro
		}
		return out[i].suite < out[j].suite
	})
	return out, nil
}

// dbScopes lists every (distro, suite) tuple with at least one row in the
// packages table. Rows whose distro or suite would not pass validateScopeName
// (control chars, path separators, reserved staging suffixes, etc.) are
// skipped: `add` validates on the way in, so a row that fails here is the
// product of external tampering or a future writer that bypasses validation,
// and we must not let publishScope materialize it as a filesystem path.
func dbScopes(ctx context.Context, r *repo.Repo) ([]scope, error) {
	rows, err := r.DB().QueryContext(ctx,
		`SELECT DISTINCT distro, suite FROM packages ORDER BY distro, suite`)
	if err != nil {
		return nil, fmt.Errorf("enumerating scopes: %w", err)
	}
	defer rows.Close()
	var out []scope
	for rows.Next() {
		var sc scope
		if err := rows.Scan(&sc.distro, &sc.suite); err != nil {
			return nil, fmt.Errorf("scanning scope: %w", err)
		}
		if validateScopeName(fieldDistro, sc.distro) != nil {
			continue
		}
		if validateScopeName(fieldSuite, sc.suite) != nil {
			continue
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating scopes: %w", err)
	}
	return out, nil
}

// filesystemScopes walks <publicDir>/<distro>/dists/<suite>/ to discover
// suites with an existing live tree on disk. The outer loop reads top-level
// <publicDir> entries and treats each directory that passes validateScopeName
// and contains a dists/ subdirectory as a candidate distro — arbitrary
// user-created top-level dirs are skipped, and the validateScopeName gate
// prevents a malformed top-level directory name from triggering atomic-swap
// operations on unrelated content. Staging siblings (.new, .old) are skipped
// at the suite level, and only directories that contain a Release file are
// treated as publish targets — without that gate, a stray directory under
// dists/ would be wiped by the atomic swap on the next publish (no flags).
func filesystemScopes(publicDir string) ([]scope, error) {
	topLevel, err := os.ReadDir(publicDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", publicDir, err)
	}
	var out []scope
	for _, d := range topLevel {
		if !d.IsDir() {
			continue
		}
		if err := validateScopeName(fieldDistro, d.Name()); err != nil {
			continue
		}
		suitesPath := filepath.Join(publicDir, d.Name(), distsDir)
		info, err := os.Stat(suitesPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", suitesPath, err)
		}
		if !info.IsDir() {
			continue
		}
		suites, err := os.ReadDir(suitesPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", suitesPath, err)
		}
		for _, s := range suites {
			if !s.IsDir() {
				continue
			}
			name := s.Name()
			if strings.HasSuffix(name, suiteSuffixNew) || strings.HasSuffix(name, suiteSuffixOld) {
				continue
			}
			if _, err := os.Stat(filepath.Join(suitesPath, name, releaseFilename)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("stat %s: %w", filepath.Join(suitesPath, name, releaseFilename), err)
			}
			if err := validateScopeName(fieldSuite, name); err != nil {
				continue
			}
			out = append(out, scope{distro: d.Name(), suite: name})
		}
	}
	return out, nil
}

// setLastPublish records the publish timestamp in the meta table so future
// commands can report when the dists/ tree was last refreshed.
func setLastPublish(ctx context.Context, r *repo.Repo, when time.Time) error {
	_, err := r.DB().ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		metaLastPublishKey, strconv.FormatInt(when.Unix(), 10))
	if err != nil {
		return fmt.Errorf("setting %s: %w", metaLastPublishKey, err)
	}
	return nil
}
