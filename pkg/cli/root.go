// Package cli wires the debrepo subcommands behind a single go-flags parser.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jessevdk/go-flags"
)

// Exit codes used across all subcommands.
const (
	ExitOK     = 0
	ExitError  = 1
	ExitUsage  = 2
	dbFilename = "repo.db"
	envRepo    = "DEBREPO_ROOT"
)

// ErrRepoNotFound is returned when the resolved repo root is missing repo.db
// and the requested command is not init.
var ErrRepoNotFound = errors.New("no repo.db found at repo root (run 'debrepo init' first)")

// validateScopeName rejects --distro / --suite / --component / --arch values
// that would let a malicious operator escape the dists/ or pool/ tree by
// embedding a path separator or `..` segment, or inject Release file headers
// via embedded newlines. Suite names additionally must not end in ".new" or
// ".old" — those suffixes are reserved for publish's staging siblings, and a
// real suite using them would be wiped by cleanStaging of a sibling publish.
// An empty value is permitted here so callers that treat the field as an
// optional filter (verify, publish) can pass it through; commands where the
// field must be set enforce non-emptiness in their own validate.
func validateScopeName(field, val string) error {
	if val == "" {
		return nil
	}
	if val == "." || val == ".." {
		return fmt.Errorf("--%s %q: dot segment not allowed", field, val)
	}
	if val[0] == '.' || val[0] == '-' {
		return fmt.Errorf("--%s %q: must not start with '.' or '-'", field, val)
	}
	for i := range len(val) {
		c := val[i]
		switch c {
		case '/', '\\', 0:
			return fmt.Errorf("--%s %q: must not contain '/', '\\', or NUL", field, val)
		}
		if c <= 0x20 || c == 0x7F {
			return fmt.Errorf("--%s %q: must not contain control characters or spaces", field, val)
		}
	}
	if field == "suite" && (strings.HasSuffix(val, ".new") || strings.HasSuffix(val, ".old")) {
		return fmt.Errorf("--%s %q: suffixes '.new' and '.old' are reserved by publish staging", field, val)
	}
	return nil
}

// requireScopeName validates val and additionally rejects an empty value. It
// is used by commands where the field is mandatory so a `--field=""` typo
// (which go-flags accepts because required:"true" only checks presence) does
// not silently produce a malformed pool tree or bypass safety guards like
// delete's whole-suite check.
func requireScopeName(field, val string) error {
	if val == "" {
		return fmt.Errorf("--%s is required and must not be empty", field)
	}
	return validateScopeName(field, val)
}

// Options holds the global flags and all subcommand groups.
type Options struct {
	Repo  string `long:"repo" short:"r" env:"DEBREPO_ROOT" description:"path to repository root (defaults to $DEBREPO_ROOT or current directory)"`
	Quiet bool   `long:"quiet" short:"q" description:"suppress non-error output"`

	Init    InitCmd    `command:"init" description:"initialize a new repository"`
	Add     AddCmd     `command:"add" description:"add one or more .deb packages to a suite"`
	Delete  DeleteCmd  `command:"delete" description:"delete packages from a suite"`
	List    ListCmd    `command:"list" description:"list packages"`
	Search  SearchCmd  `command:"search" description:"search packages by name or description"`
	Publish PublishCmd `command:"publish" description:"rebuild and sign dists/ for one or more suites"`
	Verify  VerifyCmd  `command:"verify" description:"verify pool file hashes against the database"`
	Info    InfoCmd    `command:"info" description:"inspect a .deb file without modifying the repo"`
}

// commandName returns the canonical subcommand name a parser dispatched to.
// It is used to gate the repo-root resolution rule that init does not require
// an existing repo.db.
func commandName(p *flags.Parser) string {
	if p == nil || p.Active == nil {
		return ""
	}
	return p.Active.Name
}

// Run parses argv and dispatches to the matching subcommand. It returns the
// process exit code so main can call os.Exit cleanly.
func Run(argv []string, stdout, stderr io.Writer, revision string) int {
	opts := &Options{}
	parser := flags.NewParser(opts, flags.HelpFlag|flags.PassDoubleDash)
	parser.Name = "debrepo"
	parser.LongDescription = fmt.Sprintf("Minimal Debian repository manager (revision %s).", revision)

	rest, err := parser.ParseArgs(argv)
	if err != nil {
		var fe *flags.Error
		if errors.As(err, &fe) && fe.Type == flags.ErrHelp {
			fmt.Fprintln(stdout, err.Error())
			return ExitOK
		}
		fmt.Fprintln(stderr, err.Error())
		return ExitUsage
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "unexpected positional arguments: %v\n", rest)
		return ExitUsage
	}

	cmd := commandName(parser)
	if cmd == "" {
		parser.WriteHelp(stderr)
		return ExitUsage
	}

	root, err := resolveRepoRoot(opts.Repo, cmd)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}

	ctx := &Context{
		Root:   root,
		Stdout: stdout,
		Stderr: stderr,
		Quiet:  opts.Quiet,
	}

	switch cmd {
	case "init":
		return opts.Init.Execute(ctx)
	case "add":
		return opts.Add.Execute(ctx)
	case "delete":
		return opts.Delete.Execute(ctx)
	case "list":
		return opts.List.Execute(ctx)
	case "search":
		return opts.Search.Execute(ctx)
	case "publish":
		return opts.Publish.Execute(ctx)
	case "verify":
		return opts.Verify.Execute(ctx)
	case "info":
		return opts.Info.Execute(ctx)
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n", cmd)
		return ExitUsage
	}
}

// Context carries process-wide values to subcommand handlers.
type Context struct {
	Root   string
	Stdout io.Writer
	Stderr io.Writer
	Quiet  bool
}

// resolveRepoRoot picks the repo root using flag > env > cwd, then validates
// that repo.db exists for any command other than init or info.
func resolveRepoRoot(flagRepo, cmd string) (string, error) {
	root := flagRepo
	if root == "" {
		root = os.Getenv(envRepo)
	}
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolving cwd: %w", err)
		}
		root = cwd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving repo root %q: %w", root, err)
	}
	if cmd == "init" || cmd == "info" {
		return abs, nil
	}
	if _, err := os.Stat(filepath.Join(abs, dbFilename)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrRepoNotFound, abs)
		}
		return "", fmt.Errorf("checking repo at %s: %w", abs, err)
	}
	return abs, nil
}
