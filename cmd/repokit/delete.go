package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/alkk/repokit/pkg/repo"
)

// DeleteCmd removes packages from a suite. It mutates the database only; the
// pool files it orphans are reaped by `repokit gc`, and the dists/ tree is
// rebuilt by a subsequent `repokit publish` run.
//
// At least one of --name or --sha256 is required to prevent whole-suite
// wipes. Deleting every version of a named package additionally requires
// --all-versions as a confirmation guard when --version is omitted.
type DeleteCmd struct {
	Distro      DistroName `long:"distro" description:"target distribution (plain like 'debian' or namespaced like 'customer/acme'; must be declared in config.distros)" required:"true"`
	Suite       string     `long:"suite" description:"target suite" required:"true"`
	Component   string     `long:"component" description:"filter by component"`
	Arch        string     `long:"arch" description:"filter by architecture"`
	Name        string     `long:"name" description:"filter by package name"`
	Version     string     `long:"version" description:"filter by package version"`
	SHA256      string     `long:"sha256" description:"filter by sha256 hash"`
	AllVersions bool       `long:"all-versions" description:"required confirmation when deleting every version of a named package"`
	DryRun      bool       `long:"dry-run" description:"print what would be deleted without making changes"`
}

// Execute runs the delete subcommand.
func (cmd *DeleteCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "delete: %v\n", err)
		if errors.Is(err, errDeleteUsage) {
			return exitUsage
		}
		return exitError
	}
	return exitOK
}

// errDeleteUsage flags an invalid flag combination so Execute can map it to a
// usage exit code without losing the wrapped message.
var errDeleteUsage = errors.New("invalid flag combination")

func (cmd *DeleteCmd) run(c *Context) error {
	if err := cmd.validate(); err != nil {
		return err
	}
	if err := requireConfiguredDistro(c.Config, string(cmd.Distro)); err != nil {
		return err
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	filter := repo.Filter{
		Distro:       string(cmd.Distro),
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
	if err := requireDistroScope(string(cmd.Distro)); err != nil {
		return fmt.Errorf("%w: %w", errDeleteUsage, err)
	}
	if err := requireScopeName(fieldSuite, cmd.Suite); err != nil {
		return fmt.Errorf("%w: %w", errDeleteUsage, err)
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

// runDelete removes the matching DB rows and nothing else. The pool files
// the rows referenced are left on disk as orphans (the dists/ indexes still
// point at files that still exist until the next publish); `repokit gc`
// reaps them later. The writer lock is a DeleteConn plumbing requirement —
// DeleteConn must run on a writer-locked connection — not an active
// serialization choice; SQLite already serializes writers on its own.
func (cmd *DeleteCmd) runDelete(ctx context.Context, c *Context, r *repo.Repo, filter repo.Filter) error {
	var deleted []repo.Package
	lockErr := r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
		rows, derr := r.DeleteConn(ctx, conn, filter)
		if derr != nil {
			return fmt.Errorf("deleting rows: %w", derr)
		}
		deleted = rows
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("delete transaction: %w", lockErr)
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
	fmt.Fprintf(c.Stdout,
		"deleted %d package(s) — run 'repokit publish --distro %s --suite %s' to update indexes;"+
			" orphan pool files removed via 'repokit gc'\n",
		len(rows), string(cmd.Distro), cmd.Suite)
}
