// Package cli wires the repokit subcommands behind a single go-flags parser.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alkk/repokit/pkg/config"
	"github.com/jessevdk/go-flags"
)

// Exit codes used across all subcommands.
const (
	ExitOK           = 0
	ExitError        = 1
	ExitUsage        = 2
	dbFilename       = "repo.db"
	configFilename   = "config.yaml"
	envRepo          = "REPOKIT_ROOT"
	envPublic        = "REPOKIT_PUBLIC"
	placeholderValue = "REPLACE_ME"
)

// ErrRepoNotFound is returned when the resolved repo root is missing repo.db
// and the requested command is not init.
var ErrRepoNotFound = errors.New("no repo.db found at repo root (run 'repokit init' first)")

// defaultStateDir resolves the default state directory (~/.repokit/) when no
// --repo flag or REPOKIT_ROOT env var is set. It is a package-level var so
// tests can swap it to a t.TempDir()-rooted path without touching $HOME.
var defaultStateDir = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home dir: %w", err)
	}
	return filepath.Join(home, ".repokit"), nil
}

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
	Repo   string `long:"repo" short:"r" env:"REPOKIT_ROOT" description:"path to repository state dir (defaults to $REPOKIT_ROOT or ~/.repokit/)"`
	Public string `long:"public" short:"p" env:"REPOKIT_PUBLIC" description:"path to public-facing tree (pool/ + dists/) — required for add/delete/publish/verify; falls back to public_dir in config.yaml"`
	Quiet  bool   `long:"quiet" short:"q" description:"suppress non-error output"`

	Init    InitCmd    `command:"init" description:"initialize a new repository"`
	Add     AddCmd     `command:"add" description:"add one or more .deb packages to a suite"`
	Delete  DeleteCmd  `command:"delete" description:"delete packages from a suite"`
	List    ListCmd    `command:"list" alias:"ls" description:"list packages"`
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
	parser.Name = "repokit"
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

	if needsConfig(cmd) {
		cfg, lerr := loadConfig(root)
		if lerr != nil {
			fmt.Fprintln(stderr, lerr.Error())
			return ExitError
		}
		ctx.Config = cfg
	}

	if needsPublic(cmd) {
		publicDir, perr := resolvePublicDir(opts.Public, ctx.Config, root)
		if perr != nil {
			fmt.Fprintln(stderr, perr.Error())
			return ExitError
		}
		ctx.PublicDir = publicDir
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
	Root      string
	PublicDir string
	Config    *config.Config
	Stdout    io.Writer
	Stderr    io.Writer
	Quiet     bool
}

// needsPublic reports whether cmd's handler reads or writes the public tree
// (pool/ + dists/). Commands that only touch the state dir (init, info, list,
// search) skip public-dir resolution so an unconfigured public_dir does not
// block read-only operations.
func needsPublic(cmd string) bool {
	switch cmd {
	case "add", "delete", "publish", "verify":
		return true
	default:
		return false
	}
}

// needsConfig reports whether cmd should load and validate config.yaml. init
// is the bootstrap that creates the file, and info inspects a .deb without
// touching state — both run without a config. Every other command loads it
// unconditionally so a malformed config surfaces at startup instead of
// silently passing list/search/verify and only blowing up on publish.
func needsConfig(cmd string) bool {
	switch cmd {
	case "init", "info":
		return false
	default:
		return true
	}
}

// loadConfig parses <root>/config.yaml when present. A missing file falls back
// to an empty Config so callers (resolvePublicDir, publish) get a uniform value
// to inspect rather than each branching on os.IsNotExist independently.
func loadConfig(root string) (*config.Config, error) {
	path := filepath.Join(root, configFilename)
	_, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &config.Config{Suites: map[string]config.SuiteConfig{}}, nil
		}
		return nil, fmt.Errorf("checking %s: %w", path, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

// resolvePublicDir picks the public-tree path from --public/$REPOKIT_PUBLIC
// (already merged into flagOrEnv by go-flags) with cfg.PublicDir as a fallback,
// then enforces: non-empty, not the REPLACE_ME placeholder, no control chars,
// absolute, exists as a directory, and not equal to or nested with the state
// root in either direction (a leaked nesting would put repo.db / config.yaml
// under the webserver docroot — the whole motivation for this split).
func resolvePublicDir(flagOrEnv string, cfg *config.Config, root string) (string, error) {
	candidate := flagOrEnv
	if candidate == placeholderValue {
		candidate = ""
	}
	if candidate == "" && cfg != nil && cfg.PublicDir != placeholderValue {
		candidate = cfg.PublicDir
	}
	if candidate == "" {
		return "", fmt.Errorf(
			"public_dir is not set: edit %s or pass --public / $%s",
			filepath.Join(root, configFilename), envPublic)
	}
	for _, r := range candidate {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("public_dir must not contain control characters")
		}
	}
	if !filepath.IsAbs(candidate) {
		return "", fmt.Errorf("public_dir %s: must be an absolute path", candidate)
	}
	abs := filepath.Clean(candidate)
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("public_dir %s: directory does not exist (create it manually before running)", abs)
		}
		return "", fmt.Errorf("public_dir %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("public_dir %s: not a directory", abs)
	}
	// Resolve symlinks before the nesting check so a symlink whose target is
	// inside the state dir cannot defeat the leak guard. The motivating case
	// is `--public /srv/web` being a symlink to `~/.repokit/public` — lexical
	// comparison sees siblings, the resolved paths overlap.
	rootAbs := filepath.Clean(root)
	rootResolved := evalSymlinksOrSelf(rootAbs)
	absResolved := evalSymlinksOrSelf(abs)
	if pathContainsOrEquals(rootResolved, absResolved) || pathContainsOrEquals(absResolved, rootResolved) {
		return "", fmt.Errorf(
			"public_dir %s must not equal or be nested inside state root %s",
			abs, rootAbs)
	}
	return abs, nil
}

// evalSymlinksOrSelf returns the symlink-resolved form of path, or path itself
// when EvalSymlinks fails (e.g. path doesn't exist or has a broken link). The
// caller's existing existence check already guarantees the public dir resolves;
// the state dir may legitimately be a symlink-free fresh path.
func evalSymlinksOrSelf(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// pathContainsOrEquals reports whether child is parent itself or lives under
// parent. Both paths must already be cleaned/absolute. A failed filepath.Rel
// (different volumes on Windows) is treated as "not contained".
func pathContainsOrEquals(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// resolveRepoRoot picks the state-dir root using flag > env > defaultStateDir,
// then validates that repo.db exists for any command other than init or info.
// The cwd is intentionally not consulted so resolution is predictable
// regardless of where the operator runs the binary from.
func resolveRepoRoot(flagRepo, cmd string) (string, error) {
	root := flagRepo
	if root == "" {
		root = os.Getenv(envRepo)
	}
	if root == "" {
		def, err := defaultStateDir()
		if err != nil {
			// info inspects a .deb file and never reads the state dir, and
			// init accepts a positional path that overrides the default, so
			// a missing $HOME (minimal containers / CI sandboxes without a
			// resolvable home) must not block either. The init handler
			// surfaces its own error if no positional path was given.
			if cmd == "info" || cmd == "init" {
				return "", nil
			}
			return "", err
		}
		root = def
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
