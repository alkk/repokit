package main

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
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
)

// CheckCmd checks pool files against the database: it rehashes every pool
// file a row points at and reports hash mismatches, missing pool files, and
// orphans (pool files with no DB row). Without --purge-db it is strictly
// read-only (Mode A): orphan removal lives entirely in `repokit gc`, and
// rows pointing at a missing pool file are reported but never removed. With
// --purge-db it runs writer-locked (Mode B) and deletes those missing-file
// rows; hash mismatches are still left alone because a transient FS/mount
// glitch can mimic corruption and silently dropping such rows would mask a
// real problem — the operator restores the file or runs `delete` manually.
type CheckCmd struct {
	Distro  DistroName `long:"distro" description:"limit check to one distribution (plain like 'debian' or namespaced like 'customer/acme')"`
	Suite   string     `long:"suite" description:"limit check to one suite (requires --distro)"`
	PurgeDB bool       `long:"purge-db" description:"delete database rows whose pool file is missing on disk (mismatches are left alone)"`
}

// Execute runs the check subcommand. Exit code is exitOK when the scope is
// clean (no mismatches, missing files, or orphans), otherwise exitError.
func (cmd *CheckCmd) Execute(c *Context) int {
	report, err := cmd.run(c)
	if err != nil {
		fmt.Fprintf(c.Stderr, "check: %v\n", err)
		return exitError
	}
	if report.passed() {
		return exitOK
	}
	return exitError
}

// checkReport accumulates issues found by a single check run. Paths use
// forward slashes and disk-relative form (<distro>/pool/...) across all
// three categories so a multi-distro repo (cross-distro same-suite is the
// motivating case) can be diagnosed unambiguously: two distros publishing
// the same suite share the apt-relative Filename, so reporting that form
// alone would collapse two distinct findings into identical lines.
type checkReport struct {
	Mismatches  []string
	MissingPool []string
	// Purged holds the disk paths of rows --purge-db deleted (their pool
	// file was absent). It is intentionally absent from passed(): a row
	// successfully purged is a resolved finding, so it must not fail the
	// run — mirrors gc's "the action that resolves the finding does not
	// fail the run" rule. MissingPool is moved here once the deletes commit.
	Purged  []string
	Orphans []string
}

// passed reports whether the scope is clean: no hash mismatches, no missing
// pool files, and no orphans. check is read-only, so an orphan is a finding
// the operator resolves by running `repokit gc` — its presence fails the run.
func (r *checkReport) passed() bool {
	return len(r.Mismatches) == 0 && len(r.MissingPool) == 0 && len(r.Orphans) == 0
}

func (cmd *CheckCmd) run(c *Context) (*checkReport, error) {
	if cmd.Suite != "" && cmd.Distro == "" {
		return nil, errors.New("--suite requires --distro")
	}
	if err := validateDistroScope(string(cmd.Distro)); err != nil {
		return nil, err
	}
	if err := validateScopeName(fieldSuite, cmd.Suite); err != nil {
		return nil, err
	}
	if cmd.Distro != "" {
		if err := requireConfiguredDistro(c.Config, string(cmd.Distro)); err != nil {
			return nil, err
		}
	} else if err := requireConfiguredDistros(c.Config); err != nil {
		return nil, err
	}

	var distros []string
	if c.Config != nil {
		distros = c.Config.Distros
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	report := &checkReport{}
	if cmd.PurgeDB {
		err = cmd.runPurgeDB(ctx, c, distros, r, report)
	} else {
		err = cmd.checkOnly(ctx, c.PublicDir, distros, c.Stderr, r, report)
	}
	if err != nil {
		return nil, err
	}

	cmd.printReport(c, report)
	return report, nil
}

// purgeScanHook, when non-nil, runs inside runPurgeDB's writer-locked
// callback after the missing-file rows are deleted and their disk paths
// pruned from the known set, but before the orphan scan. Production leaves
// it nil; tests set it to simulate an external process mutating the pool
// tree in that window. Mirrors the defaultStateDir override seam in main.go.
var purgeScanHook func()

// runPurgeDB is Mode B: the whole pass runs under r.WithWriterLock so the row
// snapshot, hash check, deletes, and orphan scan see a consistent DB and a
// racing add cannot slip a (row, file) pair into the gap between the list and
// the deletes (the same invariant gc relies on). Rows whose pool file is
// missing are deleted; mismatches are deliberately left for the operator.
func (cmd *CheckCmd) runPurgeDB(ctx context.Context, c *Context, distros []string, r *repo.Repo, report *checkReport) error {
	filter := repo.Filter{Distro: string(cmd.Distro), Suite: cmd.Suite}
	lockErr := r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		rows, err := r.ListConn(ctx, conn, filter)
		if err != nil {
			return fmt.Errorf("listing rows: %w", err)
		}
		rows = filterRowsByConfiguredDistros(string(cmd.Distro), rows, distros, c.Stderr)
		known, missing, err := hashAndIndexCollecting(c.PublicDir, rows, report)
		if err != nil {
			return err
		}
		for _, p := range missing {
			// Delete by the full row identity. pool.Filename embeds suite and
			// component, so the same NVRA in two suites (or components) has two
			// distinct pool files and two rows — never one shared file. Omitting
			// Suite/Component here would over-match: a row whose pool file is
			// still present (and still referenced by its published indexes) in
			// another suite would be silently deleted. Every missing row is
			// collected independently into `missing`, so a one-to-one scoped
			// delete reaps exactly what was verified absent.
			del := repo.Filter{
				Distro:       p.Distro,
				Suite:        p.Suite,
				Component:    p.Component,
				Architecture: p.Architecture,
				Name:         p.Name,
				Version:      p.Version,
			}
			if _, delErr := r.DeleteConn(ctx, conn, del); delErr != nil {
				return fmt.Errorf("purging %s/%s/%s/%s/%s/%s: %w",
					p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version, delErr)
			}
		}
		report.Purged = report.MissingPool
		report.MissingPool = nil
		// Drop every just-purged row's disk path from the known set before
		// the orphan scan. The writer lock only serializes DB writers, not
		// the filesystem: an external process (a backup restore, a re-synced
		// replica, a manual cp) can recreate a previously-missing pool file
		// in the window between the hash pass and this scan. Its row is now
		// deleted, so the reappeared file is a genuine orphan. Leaving the
		// path in known would suppress it and let the run exit clean over a
		// fresh on-disk orphan; pruning it makes the scan surface the file.
		// pool.Filename embeds suite+component, so each row owns a distinct
		// path — pruning a purged row's path never un-knows a surviving row.
		for _, p := range missing {
			delete(known, pool.DiskPath(p.Distro, p.Filename))
		}
		if purgeScanHook != nil {
			purgeScanHook()
		}
		orphans, err := scanOrphans(c.PublicDir, string(cmd.Distro), cmd.Suite, distros, known)
		if err != nil {
			return err
		}
		report.Orphans = orphans
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("purge-db pass: %w", lockErr)
	}
	return nil
}

// checkOnly hashes the rows in scope, then scans for orphans. check is
// read-only: concurrent writers may add or delete rows during the walk, so
// the report is best-effort. That is acceptable because no destructive action
// follows — `repokit gc` is the command that acts on orphans, and it takes
// the writer lock for itself.
func (cmd *CheckCmd) checkOnly(ctx context.Context, publicDir string, distros []string, stderr io.Writer, r *repo.Repo, report *checkReport) error {
	rows, err := r.List(ctx, repo.Filter{Distro: string(cmd.Distro), Suite: cmd.Suite})
	if err != nil {
		return fmt.Errorf("listing rows: %w", err)
	}
	rows = filterRowsByConfiguredDistros(string(cmd.Distro), rows, distros, stderr)
	known, err := hashAndIndex(publicDir, rows, report)
	if err != nil {
		return err
	}
	orphans, err := scanOrphans(publicDir, string(cmd.Distro), cmd.Suite, distros, known)
	if err != nil {
		return err
	}
	report.Orphans = orphans
	return nil
}

// filterRowsByConfiguredDistros drops rows whose distro is not in distros and
// emits one stderr warning per dropped distro. This is the check counterpart
// to publish's belt-and-braces filter in allScopes: if the operator removed
// a distro from config.distros after rows were inserted, hashAndIndex would
// otherwise open files under an unconfigured tree. Only applied on the
// unscoped path — when scope is set, requireConfiguredDistro already gated it.
func filterRowsByConfiguredDistros(scope string, rows []repo.Package, distros []string, stderr io.Writer) []repo.Package {
	if scope != "" {
		return rows
	}
	skipped := map[string]struct{}{}
	out := make([]repo.Package, 0, len(rows))
	for _, p := range rows {
		if !slices.Contains(distros, p.Distro) {
			skipped[p.Distro] = struct{}{}
			continue
		}
		out = append(out, p)
	}
	names := make([]string, 0, len(skipped))
	for d := range skipped {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		fmt.Fprintf(stderr,
			"check: skipping distro %q: not in config.distros (configured: %v)\n",
			d, distros)
	}
	return out
}

// hashAndIndex rehashes every row and returns a set of disk-relative paths
// (<distro>/pool/...) that the orphan walk can diff against. Mode A's
// read-only path; signature is intentionally narrower than
// hashAndIndexCollecting (no missing-row side-channel).
func hashAndIndex(publicDir string, rows []repo.Package, report *checkReport) (map[string]struct{}, error) {
	known, _, err := hashAndIndexCollecting(publicDir, rows, report)
	return known, err
}

// hashAndIndexCollecting is hashAndIndex plus the missing-row side-channel
// Mode B (--purge-db) needs: alongside the known set and the report, it
// returns every row whose pool file was absent so the caller can DeleteConn
// them. Walks the rows once so checkOne, known, and the missing slice share
// the same iteration.
func hashAndIndexCollecting(publicDir string, rows []repo.Package, report *checkReport) (map[string]struct{}, []repo.Package, error) {
	known := make(map[string]struct{}, len(rows))
	var missing []repo.Package
	for _, p := range rows {
		known[pool.DiskPath(p.Distro, p.Filename)] = struct{}{}
		absent, err := checkOne(publicDir, p, report)
		if err != nil {
			return nil, nil, err
		}
		if absent {
			missing = append(missing, p)
		}
	}
	return known, missing, nil
}

// checkOne rehashes a single pool file and records mismatches or missing
// files against report. It reports whether the pool file was absent so a
// purging caller can collect the row. A missing file is not a hard error —
// the absence is a reportable finding for the caller to decide on.
func checkOne(publicDir string, p repo.Package, report *checkReport) (absent bool, err error) {
	abs, err := pool.SafeJoin(publicDir, pool.DiskPath(p.Distro, p.Filename))
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", p.Filename, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.MissingPool = append(report.MissingPool, pool.DiskPath(p.Distro, p.Filename))
			return true, nil
		}
		return false, fmt.Errorf("opening %s: %w", p.Filename, err)
	}
	defer f.Close()

	md5h := md5.New()
	sha1h := sha1.New()
	sha256h := sha256.New()
	sha512h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(md5h, sha1h, sha256h, sha512h), f); err != nil {
		return false, fmt.Errorf("hashing %s: %w", p.Filename, err)
	}

	if !hashEq(md5h, p.MD5) || !hashEq(sha1h, p.SHA1) ||
		!hashEq(sha256h, p.SHA256) || !hashEq(sha512h, p.SHA512) {
		report.Mismatches = append(report.Mismatches, pool.DiskPath(p.Distro, p.Filename))
	}
	return false, nil
}

// hashEq compares the hex digest of h with want.
func hashEq(h hash.Hash, want string) bool {
	return hex.EncodeToString(h.Sum(nil)) == want
}

// printReport writes one line per finding plus a final status. Findings are
// always printed (even under --quiet) so a check failure stays diagnosable
// from a script.
func (cmd *CheckCmd) printReport(c *Context, report *checkReport) {
	for _, p := range report.Mismatches {
		fmt.Fprintf(c.Stdout, "mismatch %s\n", p)
	}
	for _, p := range report.MissingPool {
		fmt.Fprintf(c.Stdout, "missing %s\n", p)
	}
	for _, p := range report.Purged {
		fmt.Fprintf(c.Stdout, "purged %s\n", p)
	}
	for _, p := range report.Orphans {
		fmt.Fprintf(c.Stdout, "orphan %s\n", p)
	}
	if c.Quiet {
		return
	}
	// "always" here means "regardless of passed()": Mode B has no
	// "check: ok" branch, so when not --quiet the count line is emitted
	// even for a clean 0-purged run (like gc's "removed N"). It stays
	// gated by the --quiet guard above — --quiet suppresses every trailing
	// aggregate line tool-wide (gc, delete, check alike; see main.go's
	// "suppress non-error output"). The per-row "purged <path>" lines
	// printed above are what a script parses, so a quiet purge is still
	// scriptable; this is intentional symmetry with gc, not a contract gap.
	if cmd.PurgeDB {
		fmt.Fprintf(c.Stdout, "check: %d mismatch(es), %d purged, %d orphan(s)\n",
			len(report.Mismatches), len(report.Purged), len(report.Orphans))
		return
	}
	if report.passed() {
		fmt.Fprintln(c.Stdout, "check: ok")
		return
	}
	fmt.Fprintf(c.Stdout, "check: %d mismatch(es), %d missing, %d orphan(s)\n",
		len(report.Mismatches), len(report.MissingPool), len(report.Orphans))
}
