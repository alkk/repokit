package cli

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
)

// VerifyCmd checks pool files against the database and (optionally) garbage-
// collects pool files that have no DB row. It never removes DB rows that point
// at a missing pool file — that resolution requires a manual decision.
type VerifyCmd struct {
	Distro string `long:"distro" description:"limit verification to one distribution"`
	Suite  string `long:"suite" description:"limit verification to one suite (requires --distro)"`
	GC     bool   `long:"gc" description:"remove orphan pool files (files with no database row)"`
}

// Execute runs the verify subcommand. Exit code is ExitOK when the scope is
// clean (after any --gc removal), otherwise ExitError.
func (cmd *VerifyCmd) Execute(c *Context) int {
	report, err := cmd.run(c)
	if err != nil {
		fmt.Fprintf(c.Stderr, "verify: %v\n", err)
		return ExitError
	}
	if report.passed(cmd.GC) {
		return ExitOK
	}
	return ExitError
}

// poolDirName is the literal "pool" segment that sits under each per-distro
// disk root (i.e. <root>/<distro>/pool/...). It is no longer a top-level
// directory under <root>.
const poolDirName = "pool"

// verifyReport accumulates issues found by a single verify run. Paths use
// forward slashes and disk-relative form (<distro>/pool/...) across all
// three categories so a multi-distro repo (cross-distro same-suite is the
// motivating case) can be diagnosed unambiguously: two distros publishing
// the same suite share the apt-relative Filename, so reporting that form
// alone would collapse two distinct findings into identical lines.
type verifyReport struct {
	Mismatches  []string
	MissingPool []string
	Orphans     []string
}

// passed reports whether the scope is acceptable. Orphans only count against a
// run when --gc is absent, since --gc deletes them as part of the same call.
func (r *verifyReport) passed(gc bool) bool {
	if len(r.Mismatches) > 0 || len(r.MissingPool) > 0 {
		return false
	}
	if !gc && len(r.Orphans) > 0 {
		return false
	}
	return true
}

func (cmd *VerifyCmd) run(c *Context) (*verifyReport, error) {
	if cmd.Suite != "" && cmd.Distro == "" {
		return nil, errors.New("--suite requires --distro")
	}
	if err := validateScopeName("distro", cmd.Distro); err != nil {
		return nil, err
	}
	if err := validateScopeName("suite", cmd.Suite); err != nil {
		return nil, err
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	report := &verifyReport{}
	if cmd.GC {
		// Hold the SQLite writer lock for the entire scan-and-delete pass
		// so a concurrent `add` cannot race the orphan walk: add writes
		// its pool bytes inside its own writer-locked transaction but the
		// row only becomes visible at commit, so a gc walk that ran with
		// only a snapshot read could classify the in-flight pool file as
		// an orphan and delete it before the row appeared. BEGIN IMMEDIATE
		// serializes verify --gc against any other writer (add/delete) on
		// this repo.
		err = r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
			return cmd.verifyAndGC(ctx, c.Root, r, conn, report)
		})
		if err != nil {
			return nil, fmt.Errorf("gc pass: %w", err)
		}
	} else if err := cmd.verifyOnly(ctx, c.Root, r, report); err != nil {
		return nil, err
	}

	cmd.printReport(c, report)
	return report, nil
}

// verifyOnly runs the no-gc path: hash the rows in scope, then scan for
// orphans. Concurrent writers may add or delete rows during the walk;
// without a lock the report is best-effort, which is acceptable when no
// destructive action follows.
func (cmd *VerifyCmd) verifyOnly(ctx context.Context, root string, r *repo.Repo, report *verifyReport) error {
	rows, err := r.List(ctx, repo.Filter{Distro: cmd.Distro, Suite: cmd.Suite})
	if err != nil {
		return fmt.Errorf("listing rows: %w", err)
	}
	known, err := hashAndIndex(root, rows, report)
	if err != nil {
		return err
	}
	return cmd.scanOrphansForScope(ctx, root, r.EnumerateDistros, known, report)
}

// verifyAndGC runs the same checks as verifyOnly but reads rows through
// the writer-locked connection so the orphan set is computed against a
// snapshot no concurrent writer can mutate, then deletes the orphans
// before releasing the lock.
func (cmd *VerifyCmd) verifyAndGC(
	ctx context.Context, root string, r *repo.Repo, conn *sql.Conn, report *verifyReport,
) error {
	rows, err := r.ListConn(ctx, conn, repo.Filter{Distro: cmd.Distro, Suite: cmd.Suite})
	if err != nil {
		return fmt.Errorf("listing rows: %w", err)
	}
	known, err := hashAndIndex(root, rows, report)
	if err != nil {
		return err
	}
	enumerate := func(ctx context.Context) ([]string, error) {
		return r.EnumerateDistrosConn(ctx, conn)
	}
	if err := cmd.scanOrphansForScope(ctx, root, enumerate, known, report); err != nil {
		return err
	}
	return removeOrphans(root, report.Orphans)
}

// scanOrphansForScope dispatches the orphan walk for both the scoped
// (--distro set) and unscoped paths. The unscoped path unions DB-known
// distros with on-disk distros so a distro that exists only on disk
// (e.g. all rows deleted but pool files left behind) is still walked.
func (cmd *VerifyCmd) scanOrphansForScope(
	ctx context.Context,
	root string,
	enumerate func(context.Context) ([]string, error),
	known map[string]struct{},
	report *verifyReport,
) error {
	if cmd.Distro != "" {
		return scanOrphans(root, cmd.Distro, cmd.Suite, known, report)
	}
	dbDistros, err := enumerate(ctx)
	if err != nil {
		return fmt.Errorf("enumerating distros: %w", err)
	}
	distros, err := unionWithDiskDistros(root, dbDistros)
	if err != nil {
		return err
	}
	return scanOrphansAcrossDistros(root, distros, known, report)
}

// hashAndIndex rehashes every row and returns a set of disk-relative paths
// (<distro>/pool/...) that the orphan walk can diff against. Walks the rows
// once so verifyOne and known share the same iteration.
func hashAndIndex(root string, rows []repo.Package, report *verifyReport) (map[string]struct{}, error) {
	known := make(map[string]struct{}, len(rows))
	for _, p := range rows {
		known[pool.DiskPath(p.Distro, p.Filename)] = struct{}{}
		if err := verifyOne(root, p, report); err != nil {
			return nil, err
		}
	}
	return known, nil
}

// unionWithDiskDistros merges dbDistros with any top-level <root>/<distro>
// directory that contains a pool/ subtree. Without the disk side of the
// union, an unscoped verify would miss orphans in distros that have no DB
// rows at all — e.g. a crashed add or a delete pass that removed the last
// row but left the pool file behind. Entries whose name does not pass
// validateScopeName("distro", ...) are skipped on both sides so stray
// top-level files (repo.db, config.yaml, pubkey.asc) and arbitrary
// user-created dirs do not trigger spurious orphan walks, and a
// tampered/corrupted DB row carrying ".." or a path separator cannot
// steer the walk outside the repo root (mirrors dbScopes in publish.go).
func unionWithDiskDistros(root string, dbDistros []string) ([]string, error) {
	seen := make(map[string]struct{}, len(dbDistros))
	out := make([]string, 0, len(dbDistros))
	for _, d := range dbDistros {
		if _, ok := seen[d]; ok {
			continue
		}
		if err := validateScopeName("distro", d); err != nil {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			sort.Strings(out)
			return out, nil
		}
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := seen[name]; ok {
			continue
		}
		if err := validateScopeName("distro", name); err != nil {
			continue
		}
		poolPath := filepath.Join(root, name, poolDirName)
		info, err := os.Stat(poolPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", poolPath, err)
		}
		if !info.IsDir() {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// scanOrphansAcrossDistros walks <root>/<distro>/pool for every distro and
// records orphans into report. Used when verify runs unscoped — the union of
// all per-distro pools is the search space.
func scanOrphansAcrossDistros(root string, distros []string, known map[string]struct{}, report *verifyReport) error {
	for _, d := range distros {
		if err := scanOrphans(root, d, "", known, report); err != nil {
			return err
		}
	}
	return nil
}

// verifyOne rehashes a single pool file and records mismatches or missing
// files against report. A missing file is not a hard error — the absence is a
// reportable finding for the caller to decide on.
func verifyOne(root string, p repo.Package, report *verifyReport) error {
	abs, err := pool.SafeJoin(root, pool.DiskPath(p.Distro, p.Filename))
	if err != nil {
		return fmt.Errorf("resolving %s: %w", p.Filename, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.MissingPool = append(report.MissingPool, pool.DiskPath(p.Distro, p.Filename))
			return nil
		}
		return fmt.Errorf("opening %s: %w", p.Filename, err)
	}
	defer f.Close()

	md5h := md5.New()
	sha1h := sha1.New()
	sha256h := sha256.New()
	sha512h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(md5h, sha1h, sha256h, sha512h), f); err != nil {
		return fmt.Errorf("hashing %s: %w", p.Filename, err)
	}

	if !hashEq(md5h, p.MD5) || !hashEq(sha1h, p.SHA1) ||
		!hashEq(sha256h, p.SHA256) || !hashEq(sha512h, p.SHA512) {
		report.Mismatches = append(report.Mismatches, pool.DiskPath(p.Distro, p.Filename))
	}
	return nil
}

// hashEq compares the hex digest of h with want.
func hashEq(h hash.Hash, want string) bool {
	return hex.EncodeToString(h.Sum(nil)) == want
}

// scanOrphans walks the per-distro pool subtree narrowed by suite and records
// any regular file whose disk-relative path is not in the supplied DB set.
// The walk is a no-op when the scoped directory does not exist (e.g. a distro
// with no published files yet). distro must be non-empty; the unscoped case
// is handled by the caller iterating over distros.
func scanOrphans(root, distro, suite string, known map[string]struct{}, report *verifyReport) error {
	rel := scopePoolDir(distro, suite)
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// .tmp and .bak are transient artifacts of an in-flight or crashed
		// pool write — see pool.Replace. Treating them as orphans would let
		// --gc destroy the only recoverable copy after an interrupted
		// Replace; they are cleaned up by the next successful Replace at
		// the same path instead.
		if strings.HasSuffix(d.Name(), ".tmp") || strings.HasSuffix(d.Name(), ".bak") {
			return nil
		}
		relPath, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr //nolint:wrapcheck // surfaced verbatim by the outer wrap
		}
		key := filepath.ToSlash(relPath)
		if _, ok := known[key]; ok {
			return nil
		}
		report.Orphans = append(report.Orphans, key)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walking %s: %w", rel, walkErr)
	}
	sort.Strings(report.Orphans)
	return nil
}

// scopePoolDir returns the disk-relative subtree to walk for orphan detection
// inside a single distro. The unscoped (no distro) case has no single root —
// callers iterate over distros and call this per-distro.
func scopePoolDir(distro, suite string) string {
	if suite != "" {
		return distro + "/" + poolDirName + "/" + suite
	}
	return distro + "/" + poolDirName
}

// removeOrphans deletes the listed pool files. pool.Delete is idempotent so a
// second call would be a no-op; we still surface real I/O errors.
func removeOrphans(root string, orphans []string) error {
	for _, p := range orphans {
		if err := pool.Delete(root, p); err != nil {
			return fmt.Errorf("removing orphan %s: %w", p, err)
		}
	}
	return nil
}

// printReport writes one line per finding plus a final status. Findings are
// always printed (even under --quiet) so a verify failure stays diagnosable
// from a script.
func (cmd *VerifyCmd) printReport(c *Context, report *verifyReport) {
	for _, p := range report.Mismatches {
		fmt.Fprintf(c.Stdout, "mismatch %s\n", p)
	}
	for _, p := range report.MissingPool {
		fmt.Fprintf(c.Stdout, "missing %s\n", p)
	}
	for _, p := range report.Orphans {
		if cmd.GC {
			fmt.Fprintf(c.Stdout, "orphan %s (removed)\n", p)
			continue
		}
		fmt.Fprintf(c.Stdout, "orphan %s\n", p)
	}
	if c.Quiet {
		return
	}
	if report.passed(cmd.GC) {
		fmt.Fprintln(c.Stdout, "verify: ok")
		return
	}
	fmt.Fprintf(c.Stdout, "verify: %d mismatch(es), %d missing, %d orphan(s)\n",
		len(report.Mismatches), len(report.MissingPool), len(report.Orphans))
}
