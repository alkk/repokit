package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/alkk/repokit/pkg/deb"
	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
	flags "github.com/jessevdk/go-flags"
)

// errSkipped flags a per-deb skip in --skip-existing mode so the surrounding
// loop can advance without rolling back the transaction.
var errSkipped = errors.New("skipped")

// AddCmd stages one or more .deb packages into the (distro, suite, component)
// target. It only mutates the database and the pool tree; publishing the
// dists/ index requires a separate `repokit publish` invocation.
type AddCmd struct {
	Distro       DistroName `long:"distro" description:"target distribution (plain like 'debian' or namespaced like 'customer/acme'; must be declared in config.distros)" required:"true"`
	Suite        string     `long:"suite" description:"target suite (e.g. bookworm)" required:"true"`
	Component    string     `long:"component" default:"main" description:"target component"`
	Replace      bool       `long:"replace" description:"overwrite an existing package with the same name/version/architecture"`
	SkipExisting bool       `long:"skip-existing" description:"skip packages whose name/version/architecture is already present in the target (mutually exclusive with --replace)"`

	Args struct {
		Debs []flags.Filename `positional-arg-name:"deb" required:"1"`
	} `positional-args:"yes"`
}

// Execute runs the add subcommand.
func (cmd *AddCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "add: %v\n", err)
		return exitError
	}
	return exitOK
}

// validate enforces the flag-level invariants before any I/O — scope-name
// safety, configured-distro lookup, and the mutual-exclusion / batch-size
// constraints around --replace and --skip-existing.
func (cmd *AddCmd) validate(c *Context) error {
	if err := requireDistroScope(string(cmd.Distro)); err != nil {
		return err
	}
	for field, val := range map[string]string{
		fieldSuite: cmd.Suite, "component": cmd.Component,
	} {
		if err := requireScopeName(field, val); err != nil {
			return err
		}
	}
	if err := requireConfiguredDistro(c.Config, string(cmd.Distro)); err != nil {
		return err
	}
	if cmd.SkipExisting && cmd.Replace {
		return errors.New("--skip-existing and --replace are mutually exclusive")
	}
	if cmd.Replace && len(cmd.Args.Debs) > 1 {
		return errors.New("--replace accepts only a single .deb argument (a failed batch with --replace cannot safely roll back overwritten pool files)")
	}
	return nil
}

// run wraps the whole batch in a single SQLite transaction. On any failure
// the DB rolls back and pool files written earlier in the batch are deleted
// in reverse order to keep the on-disk tree consistent with the DB.
func (cmd *AddCmd) run(c *Context) error {
	if err := cmd.validate(c); err != nil {
		return err
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	if cmd.Replace {
		if err := cmd.reconcileBackup(ctx, c, r, string(cmd.Args.Debs[0])); err != nil {
			return fmt.Errorf("reconciling backup: %w", err)
		}
	}

	var written []string
	var backups []string
	var staleReplaced []string
	var skipped int
	txErr := r.Tx(ctx, func(tx *sql.Tx) error {
		for _, p := range cmd.Args.Debs {
			diskPath, backup, stale, perr := cmd.processOne(ctx, c, r, tx, string(p))
			if perr != nil {
				if errors.Is(perr, errSkipped) {
					skipped++
					continue
				}
				return perr
			}
			written = append(written, diskPath)
			backups = append(backups, backup)
			if stale != "" {
				staleReplaced = append(staleReplaced, stale)
			}
		}
		return nil
	})
	if txErr != nil {
		// Restore any --replace backups so the on-disk pool mirrors the
		// rolled-back DB state. RollbackReplace handles a "" backup as a
		// plain delete, so plain Write rollbacks flow through the same path.
		for i := range slices.Backward(written) {
			pool.RollbackReplace(c.PublicDir, written[i], backups[i])
		}
		return fmt.Errorf("staging batch: %w", txErr)
	}

	// Commit succeeded — the new pool bytes are durable, so the .bak from
	// any --replace can be discarded. Done after Tx returns so a commit
	// failure can still recover the prior bytes via RollbackReplace. A
	// leftover .bak from a failed cleanup would poison the next --replace
	// at the same path (see pool.stashExisting), so we warn loudly rather
	// than silently swallowing the error; the operator must remove it
	// manually before another --replace runs at this dest.
	for _, b := range backups {
		if err := pool.FinalizeReplace(b); err != nil {
			fmt.Fprintf(c.Stderr, "warning: %v — remove this file manually before the next --replace\n", err)
		}
	}

	// Sweep orphaned pool files left over when --replace points at a different
	// shard than the new path. The DB has already committed, so a failure here
	// is a benign cleanup hiccup (verify --gc reclaims the orphan); warn rather
	// than reporting the whole add as failed.
	for _, p := range staleReplaced {
		if err := pool.Delete(c.PublicDir, p); err != nil {
			fmt.Fprintf(c.Stderr, "warning: removing replaced pool file %s: %v — run 'repokit verify --gc' to reclaim\n", p, err)
		}
	}

	if !c.Quiet {
		msg := fmt.Sprintf("staged %d packages", len(cmd.Args.Debs)-skipped)
		if skipped > 0 {
			msg += fmt.Sprintf(", skipped %d", skipped)
		}
		fmt.Fprintf(c.Stdout,
			"%s in %s/%s — run 'repokit publish --distro %s --suite %s' to release\n",
			msg, string(cmd.Distro), cmd.Suite, string(cmd.Distro), cmd.Suite)
	}
	return nil
}

// processOne executes the per-item flow inside the surrounding transaction:
// inspect the .deb, optionally evict the existing row in --replace mode,
// insert the new row, then stream the bytes into the pool. Insert is run
// before the pool write so a duplicate aborts the batch without touching the
// filesystem. Returns the disk-relative pool path of the freshly written
// file so the caller can roll it back on a later failure, the .bak path
// stashed by --replace (or "" in Write mode) so the caller can
// finalize/restore it after the surrounding transaction settles, and the
// prior file's disk-relative path when --replace points at a different
// shard than the new one (e.g. the .deb's Source field changed) so the
// caller can sweep the orphan once the tx commits.
func (cmd *AddCmd) processOne(
	ctx context.Context, c *Context, r *repo.Repo, tx *sql.Tx, srcPath string,
) (string, string, string, error) {
	info, err := deb.Inspect(srcPath)
	if err != nil {
		return "", "", "", fmt.Errorf("inspecting %s: %w", srcPath, err)
	}
	filename := pool.Filename(cmd.Suite, cmd.Component,
		info.Source, info.Name, info.Version, info.Architecture)
	diskPath := pool.DiskPath(string(cmd.Distro), filename)

	pkg := repo.Package{
		Distro:       string(cmd.Distro),
		Suite:        cmd.Suite,
		Component:    cmd.Component,
		Architecture: info.Architecture,
		Name:         info.Name,
		Version:      info.Version,
		Filename:     filename,
		Size:         info.Size,
		MD5:          info.MD5,
		SHA1:         info.SHA1,
		SHA256:       info.SHA256,
		SHA512:       info.SHA512,
		Control:      info.Control,
	}

	var stale string
	if cmd.Replace {
		oldRows, derr := r.Delete(ctx, tx, repo.Filter{
			Distro: pkg.Distro, Suite: pkg.Suite, Component: pkg.Component,
			Architecture: pkg.Architecture, Name: pkg.Name, Version: pkg.Version,
		})
		if derr != nil {
			return "", "", "", fmt.Errorf("removing existing %s_%s_%s: %w",
				pkg.Name, pkg.Version, pkg.Architecture, derr)
		}
		for _, old := range oldRows {
			if old.Filename != filename {
				stale = pool.DiskPath(string(cmd.Distro), old.Filename)
				break
			}
		}
	}

	if ierr := r.Insert(ctx, tx, pkg); ierr != nil {
		if errors.Is(ierr, repo.ErrDuplicate) {
			if cmd.SkipExisting {
				cmd.reportSkip(ctx, c, r, tx, &pkg)
				return "", "", "", errSkipped
			}
			cmd.printDuplicateDiff(ctx, c, r, tx, &pkg)
			return "", "", "", fmt.Errorf("%s_%s_%s already exists in %s/%s/%s (use --replace to overwrite)",
				pkg.Name, pkg.Version, pkg.Architecture,
				pkg.Distro, pkg.Suite, pkg.Component)
		}
		return "", "", "", fmt.Errorf("inserting %s: %w", pkg.Name, ierr)
	}

	backup, werr := writePool(c.PublicDir, diskPath, srcPath, cmd.Replace)
	if werr != nil {
		return "", "", "", werr
	}
	return diskPath, backup, stale, nil
}

// reconcileBackup clears or restores a leftover <dest>.bak that survived a
// prior crashed --replace. A row with Filename = relPath is necessary but not
// sufficient evidence that dest is canonical: a crash between pool.Replace
// and the surrounding Tx commit leaves dest holding uncommitted bytes while
// SQLite rolls back to the prior row, which still references this same
// relPath when the NVRA hasn't changed. We therefore verify dest's SHA256
// against the row's before declaring committed=true. On mismatch (or when
// dest is missing), .bak holds the bytes that match the rolled-back row and
// must be restored before the next --replace runs — otherwise stashExisting
// would adopt those uncommitted bytes as recovery material and a subsequent
// rollback would silently promote them in place of the truly canonical bytes.
func (cmd *AddCmd) reconcileBackup(ctx context.Context, c *Context, r *repo.Repo, debPath string) error {
	info, err := deb.Inspect(debPath)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", debPath, err)
	}
	filename := pool.Filename(cmd.Suite, cmd.Component,
		info.Source, info.Name, info.Version, info.Architecture)
	diskPath := pool.DiskPath(string(cmd.Distro), filename)

	dest := filepath.Join(c.PublicDir, filepath.FromSlash(diskPath))
	if _, statErr := os.Lstat(dest + ".bak"); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s.bak: %w", diskPath, statErr)
	}

	rows, err := r.List(ctx, repo.Filter{
		Distro: string(cmd.Distro), Suite: cmd.Suite, Component: cmd.Component,
		Architecture: info.Architecture, Name: info.Name, Version: info.Version,
	})
	if err != nil {
		return fmt.Errorf("looking up existing %s: %w", info.Name, err)
	}
	committed := false
	for _, row := range rows {
		if row.Filename != filename {
			continue
		}
		match, herr := destMatchesSHA256(dest, row.SHA256)
		if herr != nil {
			return fmt.Errorf("verifying %s: %w", diskPath, herr)
		}
		committed = match
		break
	}
	if err := pool.ReconcileBackup(c.PublicDir, diskPath, committed); err != nil {
		return fmt.Errorf("reconciling backup for %s: %w", diskPath, err)
	}
	return nil
}

// destMatchesSHA256 reports whether the file at path hashes to want. A
// missing file is reported as no-match (not an error) so the caller can
// route a missing dest into the "restore from .bak" branch.
func destMatchesSHA256(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// reportSkip emits the stderr notice for a --skip-existing collision. When the
// existing DB row's SHA256 differs from the incoming .deb's, it switches to a
// loud warning so operators notice "same NVRA, different bytes" footguns. A
// failed r.Get is intentionally treated as a benign anomaly (r.Insert just
// proved the NVRA exists in this same tx, so a read miss is harmless) and
// degrades to the plain skip line rather than surfacing as an error.
func (cmd *AddCmd) reportSkip(
	ctx context.Context, c *Context, r *repo.Repo, tx *sql.Tx, pkg *repo.Package,
) {
	existing, err := r.Get(ctx, tx, pkg.Distro, pkg.Suite, pkg.Component,
		pkg.Architecture, pkg.Name, pkg.Version)
	if err == nil && existing != nil && existing.SHA256 != pkg.SHA256 {
		fmt.Fprintf(c.Stderr,
			"warning: skipping %s_%s_%s: NVRA matches but sha256 differs (existing=%s new=%s) — use --replace to overwrite\n",
			pkg.Name, pkg.Version, pkg.Architecture, existing.SHA256, pkg.SHA256)
		return
	}
	fmt.Fprintf(c.Stderr, "skipping %s_%s_%s: already present in %s/%s/%s\n",
		pkg.Name, pkg.Version, pkg.Architecture,
		pkg.Distro, pkg.Suite, pkg.Component)
}

// printDuplicateDiff emits the existing-vs-new sha256 diff for a duplicate
// NVRA so the operator can decide whether to re-run with --replace.
func (cmd *AddCmd) printDuplicateDiff(
	ctx context.Context, c *Context, r *repo.Repo, tx *sql.Tx, pkg *repo.Package,
) {
	existing, err := r.Get(ctx, tx, pkg.Distro, pkg.Suite, pkg.Component,
		pkg.Architecture, pkg.Name, pkg.Version)
	if err != nil || existing == nil {
		return
	}
	fmt.Fprintf(c.Stderr,
		"duplicate %s_%s_%s in %s/%s/%s\n  existing sha256: %s\n  new sha256:      %s\n",
		pkg.Name, pkg.Version, pkg.Architecture,
		pkg.Distro, pkg.Suite, pkg.Component,
		existing.SHA256, pkg.SHA256)
}

// writePool streams srcPath into the pool at relPath. Replace overwrites an
// existing destination and returns the .bak path the caller must finalize
// after the surrounding transaction commits (or restore on rollback). Write
// fails when one is already present and never produces a backup.
func writePool(root, relPath, srcPath string, replace bool) (string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", srcPath, err)
	}
	defer f.Close()
	if replace {
		backup, err := pool.Replace(root, relPath, f)
		if err != nil {
			return "", fmt.Errorf("writing pool %s: %w", relPath, err)
		}
		return backup, nil
	}
	if err := pool.Write(root, relPath, f); err != nil {
		return "", fmt.Errorf("writing pool %s: %w", relPath, err)
	}
	return "", nil
}
