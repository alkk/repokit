package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/netxms/debrepo/pkg/repo"
)

// InitCmd creates a new repository at the resolved root. The optional
// positional argument overrides the global --repo flag for ergonomics.
type InitCmd struct {
	Args struct {
		Root string `positional-arg-name:"repo-root"`
	} `positional-args:"yes"`
}

// configTemplate is the starter config.yaml written by init. It must parse
// cleanly through config.Load (signing_key non-empty, well-formed YAML).
const configTemplate = `# debrepo configuration
# Replace REPLACE_ME with the gpg key id or fingerprint that signs Release.
signing_key: "REPLACE_ME"

suites:
  # Per-suite Release header overrides keyed by <distro>/<suite>.
  # Missing fields default from the key (origin/label = distro, suite/codename = suite).
  #
  # debian/bookworm:
  #   origin: "MyRepo"
  #   label: "MyRepo"
  #   suite: "bookworm"
  #   codename: "bookworm"
  #   description: "Internal builds for Debian 12"
`

// Execute runs the init subcommand.
func (cmd *InitCmd) Execute(c *Context) int {
	root, err := cmd.resolveRoot(c.Root)
	if err != nil {
		fmt.Fprintf(c.Stderr, "init: %v\n", err)
		return ExitError
	}
	if err := initRepo(root); err != nil {
		fmt.Fprintf(c.Stderr, "init: %v\n", err)
		return ExitError
	}
	if !c.Quiet {
		fmt.Fprintf(c.Stdout, "initialized repo at %s\n", root)
	}
	return ExitOK
}

// resolveRoot picks the positional argument when provided, otherwise the
// already-resolved context root.
func (cmd *InitCmd) resolveRoot(ctxRoot string) (string, error) {
	if cmd.Args.Root == "" {
		return ctxRoot, nil
	}
	abs, err := filepath.Abs(cmd.Args.Root)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", cmd.Args.Root, err)
	}
	return abs, nil
}

// initRepo creates the on-disk layout, opens (creates) the SQLite DB to apply
// migrations, and writes a starter config.yaml. Pre-existing repo.db at root
// aborts with an error; pre-existing config.yaml is left untouched.
func initRepo(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating root %s: %w", root, err)
	}
	dbPath := filepath.Join(root, dbFilename)
	switch _, err := os.Stat(dbPath); {
	case err == nil:
		return fmt.Errorf("repo already initialized: %s exists", dbPath)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checking %s: %w", dbPath, err)
	}
	r, err := repo.Open(context.Background(), dbPath)
	if err != nil {
		return fmt.Errorf("opening db: %w", err)
	}
	if err := r.Close(); err != nil {
		return fmt.Errorf("closing db: %w", err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	switch _, err := os.Stat(cfgPath); {
	case err == nil:
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checking %s: %w", cfgPath, err)
	}
	if err := os.WriteFile(cfgPath, []byte(configTemplate), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return nil
}
