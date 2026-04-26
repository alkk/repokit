package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/netxms/debrepo/pkg/repo"
)

// SearchCmd matches packages whose name or stored control text contains the
// query substring (case-insensitive). Results render in the same table or
// line-delimited JSON formats as `list` so the two commands compose with the
// same downstream tools.
type SearchCmd struct {
	Distro string `long:"distro" description:"filter by distribution"`
	Suite  string `long:"suite" description:"filter by suite"`
	Format string `long:"format" choice:"table" choice:"json" default:"table" description:"output format"`

	Args struct {
		Query string `positional-arg-name:"query" description:"substring to match against package name or control text"`
	} `positional-args:"true"`
}

// Execute runs the search subcommand.
func (cmd *SearchCmd) Execute(c *Context) int {
	if err := cmd.run(c); err != nil {
		fmt.Fprintf(c.Stderr, "search: %v\n", err)
		if errors.Is(err, errSearchUsage) {
			return ExitUsage
		}
		return ExitError
	}
	return ExitOK
}

// errSearchUsage marks invalid invocations so Execute can map them to the
// usage exit code.
var errSearchUsage = errors.New("invalid usage")

func (cmd *SearchCmd) run(c *Context) error {
	if cmd.Args.Query == "" {
		return fmt.Errorf("%w: query argument is required", errSearchUsage)
	}

	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(c.Root, dbFilename))
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	defer r.Close()

	rows, err := r.Search(ctx, cmd.Args.Query, repo.Filter{
		Distro: cmd.Distro,
		Suite:  cmd.Suite,
	})
	if err != nil {
		return fmt.Errorf("searching packages: %w", err)
	}

	switch cmd.Format {
	case "json":
		return writePackagesJSON(c.Stdout, rows)
	default:
		return writePackagesTable(c.Stdout, rows)
	}
}
