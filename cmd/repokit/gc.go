package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
)

// GcCmd removes orphan pool files: regular files under
// <publicDir>/<distro>/pool/... that no packages row references. It never
// touches the database — `delete` is the command that retires rows; gc only
// reclaims the disk bytes those retired rows used to point at, after the
// operator has published indexes that no longer list them.
//
// gc is deliberately orphan-only and existence-based: no --name/--sha256
// surgical filters (that is `delete`'s job) and no hashing (that is
// `verify`'s job — gc only needs the set of pool paths the DB still knows).
type GcCmd struct {
	Distro DistroName `long:"distro" description:"limit gc to one distribution (plain like 'debian' or namespaced like 'customer/acme')"`
	Suite  string     `long:"suite" description:"limit gc to one suite (requires --distro)"`
	DryRun bool       `long:"dry-run" description:"print orphans that would be removed without making changes"`
}

// errGcUsage flags an invalid flag combination so Execute can map it to a
// usage exit code without losing the wrapped message.
var errGcUsage = errors.New("invalid flag combination")

// Execute runs the gc subcommand. Orphan presence is not a failure — gc is
// the action that resolves it — so both a clean run and a run that removed
// orphans exit exitOK; only validation/IO errors map to a non-zero code.
func (cmd *GcCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "gc: %v\n", err)
		if errors.Is(err, errGcUsage) {
			return exitUsage
		}
		return exitError
	}
	return exitOK
}

func (cmd *GcCmd) run(c *Context) error {
	if err := cmd.validate(); err != nil {
		return err
	}
	if cmd.Distro != "" {
		if err := requireConfiguredDistro(c.Config, string(cmd.Distro)); err != nil {
			return err
		}
	} else if err := requireConfiguredDistros(c.Config); err != nil {
		return err
	}

	var distros []string
	if c.Config != nil {
		distros = c.Config.Distros
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	filter := repo.Filter{Distro: string(cmd.Distro), Suite: cmd.Suite}
	if cmd.DryRun {
		return cmd.runDryRun(ctx, c, distros, r, filter)
	}
	return cmd.runGc(ctx, c, distros, r, filter)
}

// validate enforces the scope guards. --suite requires --distro because the
// pool tree is rooted per distro: a bare --suite has no single subtree to
// walk. Scope-name failures are usage errors (the operator must fix the flags
// before any work happens); the configured-distro gate is applied separately
// in run so an unconfigured --distro maps to exitError, matching verify.
func (cmd *GcCmd) validate() error {
	if cmd.Suite != "" && cmd.Distro == "" {
		return fmt.Errorf("%w: --suite requires --distro", errGcUsage)
	}
	if err := validateDistroScope(string(cmd.Distro)); err != nil {
		return fmt.Errorf("%w: %w", errGcUsage, err)
	}
	if err := validateScopeName(fieldSuite, cmd.Suite); err != nil {
		return fmt.Errorf("%w: %w", errGcUsage, err)
	}
	return nil
}

// runDryRun reports the orphans that would be removed without taking the
// writer lock or touching disk. A racing add cannot cause data loss here
// because nothing is deleted, so a snapshot read is sufficient.
func (cmd *GcCmd) runDryRun(ctx context.Context, c *Context, distros []string, r *repo.Repo, filter repo.Filter) error {
	refs, err := r.PoolRefs(ctx, filter)
	if err != nil {
		return fmt.Errorf("listing pool refs: %w", err)
	}
	orphans, err := cmd.scanOrphans(c.PublicDir, distros, knownPoolSet(refs))
	if err != nil {
		return err
	}
	for _, p := range orphans {
		fmt.Fprintf(c.Stdout, "would remove %s\n", p)
	}
	cmd.printSummary(c, len(orphans), true)
	return nil
}

// runGc removes orphans under a single writer lock spanning the scan and the
// unlink. The lock serializes gc against a concurrent `add`: add writes its
// pool bytes inside its own writer-locked transaction but the row only
// becomes visible at commit, so a scan that ran on a mere snapshot read could
// classify the in-flight pool file as an orphan and delete it before the row
// appeared. BEGIN IMMEDIATE blocks gc until any such writer releases.
func (cmd *GcCmd) runGc(ctx context.Context, c *Context, distros []string, r *repo.Repo, filter repo.Filter) error {
	var removed int
	lockErr := r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		refs, err := r.PoolRefsConn(ctx, conn, filter)
		if err != nil {
			return fmt.Errorf("listing pool refs: %w", err)
		}
		orphans, err := cmd.scanOrphans(c.PublicDir, distros, knownPoolSet(refs))
		if err != nil {
			return err
		}
		if err := removeOrphans(c.PublicDir, orphans, distros, c.Stdout, c.Stderr); err != nil {
			return err
		}
		removed = len(orphans)
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("gc pass: %w", lockErr)
	}
	cmd.printSummary(c, removed, false)
	return nil
}

// scanOrphans dispatches the orphan walk for both the scoped (--distro set)
// and unscoped paths. The unscoped path iterates the configured distros from
// config.yaml — repokit never walks distros the operator has not declared, so
// unrelated top-level trees under the public dir are left untouched.
func (cmd *GcCmd) scanOrphans(publicDir string, distros []string, known map[string]struct{}) ([]string, error) {
	if cmd.Distro != "" {
		return scanPoolForOrphans(publicDir, string(cmd.Distro), cmd.Suite, known)
	}
	return scanPoolForOrphansAcrossDistros(publicDir, distros, known)
}

// printSummary writes the trailing count line. Per-orphan lines are always
// printed (even under --quiet) so a gc run stays diagnosable from a script;
// only this aggregate line is suppressed by --quiet, matching verify.
func (cmd *GcCmd) printSummary(c *Context, n int, dryRun bool) {
	if c.Quiet {
		return
	}
	if dryRun {
		fmt.Fprintf(c.Stdout, "gc: would remove %d orphan(s)\n", n)
		return
	}
	fmt.Fprintf(c.Stdout, "gc: removed %d orphan(s)\n", n)
}

// knownPoolSet turns the DB's pool refs into the disk-relative path set
// (<distro>/pool/...) the orphan walk diffs against. Mirrors the key form
// produced by scanPoolForOrphans.
func knownPoolSet(refs []repo.PoolRef) map[string]struct{} {
	known := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		known[pool.DiskPath(ref.Distro, ref.Filename)] = struct{}{}
	}
	return known
}
