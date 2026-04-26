package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/netxms/debrepo/pkg/pool"
	"github.com/netxms/debrepo/pkg/repo"
)

// DeleteCmd removes packages from a suite. It mutates the database and pool
// only; the dists/ tree is rebuilt by a subsequent `debrepo publish` run.
//
// At least one of --name or --sha256 is required to prevent whole-suite
// wipes. Deleting every version of a named package additionally requires
// --all-versions as a confirmation guard when --version is omitted.
type DeleteCmd struct {
	Distro      string `long:"distro" description:"target distribution" required:"true"`
	Suite       string `long:"suite" description:"target suite" required:"true"`
	Component   string `long:"component" description:"filter by component"`
	Arch        string `long:"arch" description:"filter by architecture"`
	Name        string `long:"name" description:"filter by package name"`
	Version     string `long:"version" description:"filter by package version"`
	SHA256      string `long:"sha256" description:"filter by sha256 hash"`
	AllVersions bool   `long:"all-versions" description:"required confirmation when deleting every version of a named package"`
	DryRun      bool   `long:"dry-run" description:"print what would be deleted without making changes"`
}

// Execute runs the delete subcommand.
func (cmd *DeleteCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "delete: %v\n", err)
		if errors.Is(err, errDeleteUsage) {
			return ExitUsage
		}
		return ExitError
	}
	return ExitOK
}

// errDeleteUsage flags an invalid flag combination so Execute can map it to a
// usage exit code without losing the wrapped message.
var errDeleteUsage = errors.New("invalid flag combination")

func (cmd *DeleteCmd) run(c *Context) error {
	if err := cmd.validate(); err != nil {
		return err
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	filter := repo.Filter{
		Distro:       cmd.Distro,
		Suite:        cmd.Suite,
		Component:    cmd.Component,
		Architecture: cmd.Arch,
		Name:         cmd.Name,
		Version:      cmd.Version,
		SHA256:       cmd.SHA256,
	}

	if cmd.DryRun {
		return cmd.runDryRun(ctx, c, r, filter)
	}
	return cmd.runDelete(ctx, c, r, filter)
}

// validate enforces the safety guards on flag combinations that exist to keep
// a typo from wiping a whole suite. The repo layer also rejects an empty
// filter, but its message is opaque to the user — surface a friendlier one
// here.
func (cmd *DeleteCmd) validate() error {
	for field, val := range map[string]string{
		"distro": cmd.Distro, "suite": cmd.Suite,
	} {
		if err := requireScopeName(field, val); err != nil {
			return fmt.Errorf("%w: %w", errDeleteUsage, err)
		}
	}
	for field, val := range map[string]string{
		"component": cmd.Component, "arch": cmd.Arch,
	} {
		if err := validateScopeName(field, val); err != nil {
			return fmt.Errorf("%w: %w", errDeleteUsage, err)
		}
	}
	if cmd.Name == "" && cmd.SHA256 == "" {
		return fmt.Errorf("%w: at least one of --name or --sha256 is required", errDeleteUsage)
	}
	if cmd.Name != "" && cmd.Version == "" && cmd.SHA256 == "" && !cmd.AllVersions {
		return fmt.Errorf("%w: --name without --version requires --all-versions to confirm", errDeleteUsage)
	}
	return nil
}

// runDryRun lists matching rows without altering the DB or pool.
func (cmd *DeleteCmd) runDryRun(ctx context.Context, c *Context, r *repo.Repo, filter repo.Filter) error {
	rows, err := r.List(ctx, filter)
	if err != nil {
		return fmt.Errorf("listing matches: %w", err)
	}
	cmd.report(c, rows, true)
	return nil
}

// runDelete removes the DB rows and their pool files under a single writer
// lock. The lock spans both phases so a concurrent `add` (which writes its
// pool bytes inside its own writer-locked transaction) cannot insert a row
// referencing one of these filenames between our DB commit and the pool
// unlink — without serialization, the pool.Delete loop would silently
// remove the freshly written bytes belonging to that committed row. A
// crash between the DB commit and a still-pending pool.Delete leaves only
// orphans (pool files with no DB row), which `verify --gc` recovers; the
// reverse order would leave DB rows pointing at missing files, which is
// harder to repair, so partial pool failures keep the DB committed and
// surface the unlink error to the operator.
func (cmd *DeleteCmd) runDelete(ctx context.Context, c *Context, r *repo.Repo, filter repo.Filter) error {
	var (
		deleted    []repo.Package
		poolErr    error
		poolErrIdx int
	)
	lockErr := r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		rows, derr := r.DeleteConn(ctx, conn, filter)
		if derr != nil {
			return fmt.Errorf("deleting rows: %w", derr)
		}
		deleted = rows
		for i, p := range deleted {
			if err := pool.Delete(c.Root, p.Filename); err != nil {
				poolErr = err
				poolErrIdx = i
				//nolint:nilerr // intentional: keep the DB delete committed so partial pool failures leave only orphans (recoverable via verify --gc), not rows pointing at missing files
				return nil
			}
		}
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("delete transaction: %w", lockErr)
	}
	if poolErr != nil {
		cmd.report(c, deleted[:poolErrIdx], false)
		return fmt.Errorf("removing pool file %s (%d of %d removed before failure): %w",
			deleted[poolErrIdx].Filename, poolErrIdx, len(deleted), poolErr)
	}

	cmd.report(c, deleted, false)
	return nil
}

// report prints a one-line-per-package summary plus a final count. dryRun
// changes the verb so the operator can tell at a glance which mode ran, and
// suppresses the publish hint since nothing has actually changed.
func (cmd *DeleteCmd) report(c *Context, rows []repo.Package, dryRun bool) {
	if c.Quiet {
		return
	}
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	for _, p := range rows {
		fmt.Fprintf(c.Stdout, "%s %s_%s_%s (%s)\n", verb, p.Name, p.Version, p.Architecture, p.Filename)
	}
	if dryRun {
		fmt.Fprintf(c.Stdout, "would delete %d package(s) (dry run, no changes made)\n", len(rows))
		return
	}
	fmt.Fprintf(c.Stdout, "deleted %d package(s) — run 'debrepo publish --distro %s --suite %s' to update indexes\n",
		len(rows), cmd.Distro, cmd.Suite)
}
