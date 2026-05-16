package main

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
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

// VerifyCmd checks pool files against the database: it rehashes every pool
// file a row points at and reports hash mismatches, missing pool files, and
// orphans (pool files with no DB row). It is strictly read-only — orphan
// removal lives entirely in `repokit gc`, and it never removes DB rows that
// point at a missing pool file (that resolution requires a manual decision).
type VerifyCmd struct {
	Distro DistroName `long:"distro" description:"limit verification to one distribution (plain like 'debian' or namespaced like 'customer/acme')"`
	Suite  string     `long:"suite" description:"limit verification to one suite (requires --distro)"`
}

// Execute runs the verify subcommand. Exit code is exitOK when the scope is
// clean (no mismatches, missing files, or orphans), otherwise exitError.
func (cmd *VerifyCmd) Execute(c *Context) int {
	report, err := cmd.run(c)
	if err != nil {
		fmt.Fprintf(c.Stderr, "verify: %v\n", err)
		return exitError
	}
	if report.passed() {
		return exitOK
	}
	return exitError
}

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

// passed reports whether the scope is clean: no hash mismatches, no missing
// pool files, and no orphans. verify is read-only, so an orphan is a finding
// the operator resolves by running `repokit gc` — its presence fails the run.
func (r *verifyReport) passed() bool {
	return len(r.Mismatches) == 0 && len(r.MissingPool) == 0 && len(r.Orphans) == 0
}

func (cmd *VerifyCmd) run(c *Context) (*verifyReport, error) {
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

	report := &verifyReport{}
	if err := cmd.verifyOnly(ctx, c.PublicDir, distros, c.Stderr, r, report); err != nil {
		return nil, err
	}

	cmd.printReport(c, report)
	return report, nil
}

// verifyOnly hashes the rows in scope, then scans for orphans. verify is
// read-only: concurrent writers may add or delete rows during the walk, so
// the report is best-effort. That is acceptable because no destructive action
// follows — `repokit gc` is the command that acts on orphans, and it takes
// the writer lock for itself.
func (cmd *VerifyCmd) verifyOnly(ctx context.Context, publicDir string, distros []string, stderr io.Writer, r *repo.Repo, report *verifyReport) error {
	rows, err := r.List(ctx, repo.Filter{Distro: string(cmd.Distro), Suite: cmd.Suite})
	if err != nil {
		return fmt.Errorf("listing rows: %w", err)
	}
	rows = filterRowsByConfiguredDistros(string(cmd.Distro), rows, distros, stderr)
	known, err := hashAndIndex(publicDir, rows, report)
	if err != nil {
		return err
	}
	// scoped (--distro set) walks one per-distro pool subtree; unscoped
	// iterates the configured distros from config.yaml — repokit never walks
	// distros the operator has not declared, so unrelated top-level trees
	// under the public dir are left untouched.
	var orphans []string
	if cmd.Distro != "" {
		orphans, err = scanPoolForOrphans(publicDir, string(cmd.Distro), cmd.Suite, known)
	} else {
		orphans, err = scanPoolForOrphansAcrossDistros(publicDir, distros, known)
	}
	if err != nil {
		return err
	}
	report.Orphans = orphans
	return nil
}

// filterRowsByConfiguredDistros drops rows whose distro is not in distros and
// emits one stderr warning per dropped distro. This is the verify counterpart
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
			"verify: skipping distro %q: not in config.distros (configured: %v)\n",
			d, distros)
	}
	return out
}

// hashAndIndex rehashes every row and returns a set of disk-relative paths
// (<distro>/pool/...) that the orphan walk can diff against. Walks the rows
// once so verifyOne and known share the same iteration.
func hashAndIndex(publicDir string, rows []repo.Package, report *verifyReport) (map[string]struct{}, error) {
	known := make(map[string]struct{}, len(rows))
	for _, p := range rows {
		known[pool.DiskPath(p.Distro, p.Filename)] = struct{}{}
		if err := verifyOne(publicDir, p, report); err != nil {
			return nil, err
		}
	}
	return known, nil
}

// verifyOne rehashes a single pool file and records mismatches or missing
// files against report. A missing file is not a hard error — the absence is a
// reportable finding for the caller to decide on.
func verifyOne(publicDir string, p repo.Package, report *verifyReport) error {
	abs, err := pool.SafeJoin(publicDir, pool.DiskPath(p.Distro, p.Filename))
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
		fmt.Fprintf(c.Stdout, "orphan %s\n", p)
	}
	if c.Quiet {
		return
	}
	if report.passed() {
		fmt.Fprintln(c.Stdout, "verify: ok")
		return
	}
	fmt.Fprintf(c.Stdout, "verify: %d mismatch(es), %d missing, %d orphan(s)\n",
		len(report.Mismatches), len(report.MissingPool), len(report.Orphans))
}
