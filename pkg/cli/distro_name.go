package cli

import (
	"os"
	"strings"

	"github.com/jessevdk/go-flags"
)

// DistroName is a string alias used as the type of every --distro flag so
// go-flags' completion engine can call Complete on it. The underlying value is
// still a plain string and is cast back at every read site.
type DistroName string

// Complete returns the configured distros that share the given prefix. It is
// invoked by go-flags during TAB completion (GO_FLAGS_COMPLETION=1) and must
// stay silent on every error — writing diagnostics to stderr in this path
// corrupts the shell's completion display.
func (d *DistroName) Complete(match string) []flags.Completion {
	root := completionStateDir()
	if root == "" {
		return nil
	}
	cfg, err := loadConfig(root)
	if err != nil || cfg == nil {
		return nil
	}
	var out []flags.Completion
	for _, name := range cfg.Distros {
		if match == "" || strings.HasPrefix(name, match) {
			out = append(out, flags.Completion{Item: name})
		}
	}
	return out
}

// completionStateDir resolves the state dir using the same flag > env >
// defaultStateDir precedence as resolveRepoRoot, but reads --repo / -r from
// os.Args because Options.Repo is not yet populated when go-flags invokes
// Complete during completion mode. Returns "" when no source yields a path.
func completionStateDir() string {
	args := os.Args
	if len(args) > 0 {
		args = args[1:]
	}
	if v := lookupRepoFlag(args); v != "" {
		return v
	}
	if v := os.Getenv(envRepo); v != "" {
		return v
	}
	v, err := defaultStateDir()
	if err != nil {
		return ""
	}
	return v
}

const (
	flagRepoLong    = "--repo"
	flagRepoLongEq  = "--repo="
	flagRepoShort   = "-r"
	flagRepoShortE  = "-r="
	flagPublicLong  = "--public"
	flagPublicShort = "-p"
)

// lookupRepoFlag scans args for a --repo / -r flag value. It accepts the five
// forms go-flags itself accepts: --repo=VALUE, --repo VALUE, -r=VALUE,
// -r VALUE, and -rVALUE (short adjacent form). The scan continues past
// positional tokens because go-flags accepts global flags both before and
// after the subcommand name. Returns "" when the flag is absent or when it
// appears as the last token with no value yet typed.
func lookupRepoFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return ""
		case a == flagRepoLong || a == flagRepoShort:
			if i+1 >= len(args) {
				return ""
			}
			next := args[i+1]
			if strings.HasPrefix(next, "-") {
				return ""
			}
			return next
		case strings.HasPrefix(a, flagRepoLongEq):
			return strings.TrimPrefix(a, flagRepoLongEq)
		case strings.HasPrefix(a, flagRepoShortE) && len(a) >= 3:
			return strings.TrimPrefix(a, flagRepoShortE)
		case strings.HasPrefix(a, flagRepoShort) && len(a) > 2 && a[2] != '-':
			return a[2:]
		case a == flagPublicLong || a == flagPublicShort:
			// --public / -p takes a separate value; consume it so the path
			// is not mistaken for a positional and we keep looking for --repo.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			// any other flag or positional — keep scanning, since go-flags
			// accepts global flags both before and after the subcommand name.
			continue
		}
	}
	return ""
}
