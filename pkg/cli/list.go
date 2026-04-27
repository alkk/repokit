package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/alkk/repokit/pkg/repo"
)

// ListCmd prints packages matching the supplied filters in either a
// human-friendly aligned table or line-delimited JSON for scripting.
type ListCmd struct {
	Distro    string `long:"distro" description:"filter by distribution"`
	Suite     string `long:"suite" description:"filter by suite"`
	Component string `long:"component" description:"filter by component"`
	Arch      string `long:"arch" description:"filter by architecture"`
	Name      string `long:"name" description:"filter by package name"`
	Format    string `long:"format" choice:"table" choice:"json" default:"table" description:"output format"`
}

// Execute runs the list subcommand.
func (cmd *ListCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "list: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func (cmd *ListCmd) run(c *Context) error {
	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	rows, err := r.List(ctx, repo.Filter{
		Distro:       cmd.Distro,
		Suite:        cmd.Suite,
		Component:    cmd.Component,
		Architecture: cmd.Arch,
		Name:         cmd.Name,
	})
	if err != nil {
		return fmt.Errorf("querying packages: %w", err)
	}

	switch cmd.Format {
	case "json":
		return writePackagesJSON(c.Stdout, rows)
	default:
		return writePackagesTable(c.Stdout, rows)
	}
}

// listEntry is the JSON shape emitted by `list --format json`. Field order is
// captured by the struct definition so output is stable for jq pipelines.
type listEntry struct {
	Distro       string `json:"distro"`
	Suite        string `json:"suite"`
	Component    string `json:"component"`
	Architecture string `json:"architecture"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

// writePackagesJSON emits one JSON object per row terminated by `\n`. An empty
// result set produces no output (jq-friendly).
func writePackagesJSON(w io.Writer, rows []repo.Package) error {
	enc := json.NewEncoder(w)
	for _, p := range rows {
		if err := enc.Encode(listEntry{
			Distro:       p.Distro,
			Suite:        p.Suite,
			Component:    p.Component,
			Architecture: p.Architecture,
			Name:         p.Name,
			Version:      p.Version,
			Filename:     p.Filename,
			Size:         p.Size,
			SHA256:       p.SHA256,
		}); err != nil {
			return fmt.Errorf("encoding row: %w", err)
		}
	}
	return nil
}

// writePackagesTable emits an aligned, header-prefixed table. An empty result
// set produces no output (mirrors the JSON case for predictable scripting).
func writePackagesTable(w io.Writer, rows []repo.Package) error {
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "DISTRO\tSUITE\tCOMPONENT\tARCH\tNAME\tVERSION\tSIZE"); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}
	for _, p := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version, p.Size); err != nil {
			return fmt.Errorf("writing row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flushing table: %w", err)
	}
	return nil
}
