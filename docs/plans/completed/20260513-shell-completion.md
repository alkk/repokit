# Shell completion (bash, zsh, fish)

## Overview

Add shell-completion support for `repokit` so users can TAB-complete subcommand
names, flag names, file paths to `.deb` arguments, and `--distro` values from
`config.yaml`. Uses `jessevdk/go-flags`' built-in completion machinery (env var
`GO_FLAGS_COMPLETION`) plus a tiny per-shell wiring script. No new runtime
dependency.

A new `repokit completion <shell>` subcommand emits the wiring script for the
requested shell to stdout. Users redirect it into the shell's completion
directory once and TAB just works thereafter.

## Context (from discovery)

- CLI is dispatched in `pkg/cli/root.go` through a single `Options` struct with
  per-subcommand fields. Adding a subcommand requires: a struct on `Options`,
  a `case` in the dispatch switch, and a `<name>.go` handler. Each subcommand
  `Execute` writes through `Context` (state dir, public dir, config, stdout).
- `needsPublic` and `needsConfig` in `root.go` gate which subcommands resolve
  public dir / load config. Completion needs neither.
- Existing `--distro` fields on `AddCmd`, `DeleteCmd`, `PublishCmd`,
  `VerifyCmd`, `InfoCmd`, `ListCmd`, `SearchCmd` are plain `string`. The four
  write commands validate via `validateDistroScope` / `requireDistroScope`.
- go-flags' `Completer` interface (`vendor/.../go-flags/completion.go`) is
  type-based: a flag's underlying type must define
  `Complete(match string) []flags.Completion`. So enabling dynamic completion
  on `--distro` requires a new named string type used across the subcommand
  structs.
- `flags.Filename` is a marker type recognised by go-flags' completion engine
  (`vendor/.../go-flags/completion.go:49`); fields typed as `flags.Filename`
  cause completion to emit a `filename` sentinel that shell wrappers translate
  into native file completion.
- **Default completion path calls `os.Exit(0)`**: `vendor/.../go-flags/parser.go:233-234`
  shows that when `GO_FLAGS_COMPLETION` is set and no `CompletionHandler` is
  installed, go-flags writes items via `fmt.Println` to **process stdout** then
  calls `os.Exit(0)`. We must install a custom `parser.CompletionHandler`
  (`parser.go:44-47`) so completions go to the `io.Writer` passed into
  `cli.Run` and the function can return normally — otherwise integration
  tests cannot capture output and the process would terminate mid-test.
- **`choice:` tag does not apply to positional args**: `option.go:66` defines
  `Choices` on `Option` only; `arg.go` has no Choices field and no validator.
  A `choice:"bash"` tag on a positional struct field is silently ignored. The
  completion subcommand must validate the shell value manually in `Execute`.
- `defaultStateDir` is already a package-level `var` so tests can swap it.
- Integration tests pattern: `pkg/cli/integration_test.go`, `t.TempDir()` for
  state + public, `cli.Run(...)` invoked directly with custom argv.

## Development Approach

- **testing approach**: Regular (code first, then tests in same task — matches
  existing project style).
- Complete each task fully before moving to the next.
- Every task ends with new/updated tests for code added or modified in that
  task — both success and error paths.
- All tests must pass before starting the next task.
- Run `make test` after each task.
- Update this plan file inline when scope changes; mark completed checkboxes
  with `[x]` as work lands.

## Testing Strategy

- **unit tests**: required per task; same-name `*_test.go` siblings using
  `testify`, table-driven where there's more than one case.
- **integration**: one new case in `pkg/cli/integration_test.go` that invokes
  `cli.Run` with `GO_FLAGS_COMPLETION=1` set in the env. Requires Task 1's
  `CompletionHandler` wiring so output is captured via the `io.Writer` param
  instead of process stdout, and so `Run` returns instead of `os.Exit`-ing.
- Mocks: none needed; all new code is pure or filesystem-backed and tested
  against `t.TempDir()` fixtures.

## Progress Tracking

- Mark completed items with `[x]` immediately.
- ➕ prefix for newly discovered tasks.
- ⚠️ prefix for blockers.
- On full completion, move plan to `docs/plans/completed/`.

## Solution Overview

Four pieces wired together:

1. **`parser.CompletionHandler` in `cli.Run`** — installed before
   `parser.ParseArgs` so completion items are written to the `stdout`
   `io.Writer` parameter and `Run` returns `ExitOK` instead of letting
   go-flags call `os.Exit(0)`. This is the testability prerequisite.
2. **Dynamic `--distro` completion** — new exported type `cli.DistroName`
   (alias of `string`) implementing
   `Complete(match string) []flags.Completion`. Every subcommand struct with
   a `--distro` field switches `string` → `DistroName`; call sites cast to
   `string`. Errors during completion are swallowed silently (stderr noise
   corrupts shell completion output).
3. **`completion` subcommand** — `pkg/cli/completion.go`. Positional
   `shell` arg validated manually in `Execute` (go-flags' `choice:` tag does
   not apply to positionals). Handler writes the embedded script for the
   chosen shell to `c.Stdout`. Excluded from `needsPublic` and `needsConfig`;
   short-circuits `resolveRepoRoot` so completion never touches the state
   dir.
4. **Embedded shell wrappers** — `pkg/cli/completion/{bash,zsh,fish}.sh`,
   loaded into a `map[string]string` via `//go:embed`. Each wrapper sets
   `GO_FLAGS_COMPLETION=1` and re-invokes `repokit` with the current command
   line, then feeds the result back into the shell's completion machinery.

## Technical Details

### Task ordering rationale

`CompletionHandler` wiring (Task 1) lands first because every later task
depends on it for tests to work — without it, any test that invokes `cli.Run`
under `GO_FLAGS_COMPLETION=1` would call `os.Exit(0)` and terminate the test
binary. The `DistroName` type lands second so the field-type migration
(Task 3) can compile-check immediately. The shell wrappers and subcommand
(Task 4) come after the type infrastructure is solid.

### `parser.CompletionHandler` (Task 1)

Wire a custom handler in `Run` **before** `parser.ParseArgs`:

```go
parser := flags.NewParser(opts, flags.HelpFlag|flags.PassDoubleDash)
parser.CompletionHandler = func(items []flags.Completion) {
    for _, it := range items {
        fmt.Fprintln(stdout, it.Item)
    }
}
```

The handler is invoked only when go-flags is in completion mode (per
`parser.go:230`). In that mode, `parser.ParseArgs` returns `(nil, nil)` after
the handler runs, so `Run` falls through to `commandName(parser) == ""`. We
must short-circuit there: detect completion mode via
`os.Getenv("GO_FLAGS_COMPLETION") != ""` and return `ExitOK` before the
"no subcommand → write help" branch fires.

### `DistroName` (Task 2)

```go
type DistroName string

func (d *DistroName) Complete(match string) []flags.Completion {
    root := completionStateDir()
    if root == "" { return nil }
    cfg, err := loadConfig(root)
    if err != nil || cfg == nil { return nil }
    var out []flags.Completion
    for _, name := range cfg.Distros {
        if match == "" || strings.HasPrefix(name, match) {
            out = append(out, flags.Completion{Item: name})
        }
    }
    return out
}
```

`completionStateDir` mirrors `resolveRepoRoot` precedence using `os.Args`
because `Options.Repo` is not populated during completion mode:

```go
func completionStateDir() string {
    if v := lookupRepoFlag(os.Args); v != "" { return v }
    if v := os.Getenv(envRepo); v != "" { return v }
    if v, err := defaultStateDir(); err == nil { return v }
    return ""
}
```

`lookupRepoFlag(args []string) string` parses these forms in order of
appearance:

- `--repo=VALUE`
- `--repo VALUE` (separate token, only consumed if the next token is not
  `--`-prefixed)
- `-r=VALUE`
- `-r VALUE` (separate token, same rule)
- `-rVALUE` (short adjacent form — go-flags supports it for short flags
  that take a value)

Returns `""` if `--repo` / `-r` is the last token (no value yet typed) or
absent. Stops scanning at the first positional (non-flag token that isn't
following a value-consuming flag). Pure function, easy to test.

### `completion` subcommand (Task 4)

```go
type CompletionCmd struct {
    Args struct {
        Shell string `positional-arg-name:"shell" required:"yes"`
    } `positional-args:"yes"`
}

func (cmd *CompletionCmd) Execute(c *Context) int {
    script, ok := completionScripts[cmd.Args.Shell]
    if !ok {
        fmt.Fprintf(c.Stderr,
            "completion: unsupported shell %q (valid: bash, zsh, fish)\n",
            cmd.Args.Shell)
        return ExitUsage
    }
    fmt.Fprint(c.Stdout, script)
    return ExitOK
}
```

Manual validation in `Execute` instead of `choice:` tag — see Context section
above for why.

### Embed bundle (Task 4)

```go
//go:embed completion/bash.sh completion/zsh.sh completion/fish.sh
var completionFS embed.FS

var completionScripts = map[string]string{
    "bash": mustReadCompletion("completion/bash.sh"),
    "zsh":  mustReadCompletion("completion/zsh.sh"),
    "fish": mustReadCompletion("completion/fish.sh"),
}
```

`mustReadCompletion` panics on a missing embedded file — a build-time
guarantee.

### Shell wrappers (Task 4)

Final wording cross-checked against
`vendor/github.com/jessevdk/go-flags/completion.go` (esp. line 312, where
items are printed one per line; and the `filename` sentinel handling for
`flags.Filename` fields).

**bash.sh**:
```bash
_repokit() {
    local IFS=$'\n'
    COMPREPLY=($(GO_FLAGS_COMPLETION=1 "${COMP_WORDS[0]}" "${COMP_WORDS[@]:1}" 2>/dev/null))
}
complete -F _repokit repokit
```

**zsh.sh**:
```zsh
#compdef repokit
_repokit() {
    local -a completions
    completions=("${(@f)$(GO_FLAGS_COMPLETION=1 repokit "${words[@]:1}" 2>/dev/null)}")
    compadd -- "${completions[@]}"
}
compdef _repokit repokit
```

**fish.sh** — uses `commandline -opc` minus the first token (the binary
name as typed, which may be a path) rather than a fixed `[2..]` index:
```fish
function __repokit_complete
    set -l tokens (commandline -opc)
    set -l args $tokens[2..-1]
    env GO_FLAGS_COMPLETION=1 $tokens[1] $args 2>/dev/null
end
complete -c repokit -f -a '(__repokit_complete)'
```

Each wrapper ≤ 20 lines.

### `Distro` field migration (Task 3)

Change `Distro string` → `Distro DistroName` in the seven subcommand structs
that carry it: `AddCmd`, `DeleteCmd`, `PublishCmd`, `VerifyCmd`, `InfoCmd`,
`ListCmd`, `SearchCmd`. All seven get TAB completion on `--distro` after the
migration — there is no per-command opt-out and excluding read-only commands
would be inconsistent.

Concrete call sites that need `string(cmd.Distro)` casts (verified by
`grep -n 'cmd\.Distro' pkg/cli/*.go`; re-grep at task start to catch any
drift before editing):

- `pkg/cli/add.go`: lines 47, 57, 130 (twice), 155, 158, 185, **226**, 237
- `pkg/cli/delete.go`: lines 52, 64, 84, 181
- `pkg/cli/publish.go`: lines 57, 63, 64, 117, 118, 120
- `pkg/cli/verify.go`: lines 83, 90, 138, 142, 157, **161**, **216**
- `pkg/cli/info.go`: lines 34, 64
- `pkg/cli/list.go`: line 43 (inside `repo.Filter{Distro: cmd.Distro, ...}`)
- `pkg/cli/search.go`: line 55 (inside `repo.Filter{Distro: cmd.Distro, ...}`)

`repo.Filter.Distro` is `string`, so `Filter{Distro: cmd.Distro}` will fail
to compile after the type change unless cast. Empty-string comparisons
(`cmd.Distro != ""`, `cmd.Distro == ""` at e.g. `verify.go:80, 89, 215`,
`publish.go:63, 117, 120`, `info.go:61`) work without casts because Go
compares named-string types against untyped string literals.

When the resolved state dir has no `repo.db` or `config.yaml`,
`loadConfig` returns an empty `*config.Config` (per `root.go:327`), so
`DistroName.Complete` produces no completions and exits silently — the
correct behaviour for a fresh machine with no repo yet.

Tests that build subcommand structs via flag parsing (every existing test
calling `cli.Run([]string{...})`) need no changes — go-flags assigns through
reflection and handles the conversion. Tests that set `cmd.Distro = "..."`
directly need `cmd.Distro = DistroName("...")`. The migration is a pure
refactor with no behavior change.

### `.deb` positional (Task 5)

`AddCmd.Args.Debs` is `[]string` today. Change to `[]flags.Filename` so the
completion engine emits the file-completion sentinel. The runtime code that
reads each path accepts a string; cast at the loop site:
`cmd.processOne(ctx, c, r, tx, string(p))`.

Same for `InfoCmd`'s positional `.deb` field.

Discover affected tests before editing:
`grep -n "Args.Debs\|Args.Deb\b" pkg/cli/*_test.go`.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes below): everything in this
  codebase — new files, type changes, tests, docs.
- **Post-Completion** (no checkboxes): manual `make build` + smoke test of
  TAB completion in a real shell. Documented at the bottom.

## Implementation Steps

### Task 1: Wire `parser.CompletionHandler` in `cli.Run`

**Files:**
- Modify: `pkg/cli/root.go`
- Modify: `pkg/cli/root_test.go`

- [x] in `Run`, set `parser.CompletionHandler` after `flags.NewParser` and
      before `parser.ParseArgs`; handler iterates items and writes
      `item.Item` plus newline to the `stdout` writer
- [x] detect completion mode (`os.Getenv("GO_FLAGS_COMPLETION") != ""`)
      after `parser.ParseArgs` returns; if set, return `ExitOK` without
      falling through to the "no subcommand → write help" branch
- [x] add a test in `root_test.go` that calls `Run` with
      `GO_FLAGS_COMPLETION=1` (via `t.Setenv`) and argv listing partial
      subcommand name (e.g. `[]string{"i"}`); assert stdout buffer
      contains `info` and `init`, stderr is empty, exit is `ExitOK`
- [x] add a second test confirming non-completion mode behaves identically
      to before (no regression for normal subcommand dispatch)
- [x] run `make test` — must pass before Task 2

### Task 2: Add `DistroName` type with dynamic completion

**Files:**
- Create: `pkg/cli/distro_name.go`
- Create: `pkg/cli/distro_name_test.go`

- [x] define `type DistroName string` in `pkg/cli/distro_name.go`
- [x] implement `lookupRepoFlag(args []string) string` covering all five
      forms specified in Technical Details (`--repo=X`, `--repo X`,
      `-r=X`, `-r X`, `-rX`); stop scanning at first positional; return
      `""` if `--repo` / `-r` is the last token
- [x] implement `completionStateDir() string` chaining flag → env →
      `defaultStateDir`, never returning an error
- [x] implement `(d *DistroName) Complete(match string) []flags.Completion`
      that loads config from the resolved state dir and returns filtered
      distros; returns nil on any error (silent during TAB completion)
- [x] write table-driven tests for `lookupRepoFlag` (each of the five
      forms, missing flag, last-token case, stops at first positional)
- [x] write tests for `Complete`: empty match returns all configured
      distros; prefix match filters; namespaced distro
      (`customer/acme`) round-trips; missing config returns nil; missing
      state dir returns nil; malformed config returns nil
- [x] override `defaultStateDir` in tests to point at a `t.TempDir()`
      fixture; verify `--repo` arg wins over env var
- [x] run `make test` — must pass before Task 3

### Task 3: Migrate `Distro` fields to `DistroName`

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/delete.go`
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/verify.go`
- Modify: `pkg/cli/info.go`
- Modify: `pkg/cli/list.go`
- Modify: `pkg/cli/search.go`
- Modify: any `*_test.go` siblings that set `Distro:` literally (discover
  via `grep -n 'Distro: *"' pkg/cli/*_test.go` before editing)

- [x] change `Distro string` to `Distro DistroName` in all seven
      subcommand structs (keep all struct tags identical)
- [x] add `string(cmd.Distro)` casts at every concrete call site listed
      in the Technical Details section (verified by grep); each cast
      should wrap the read so the diff is reviewable
- [x] confirm `repo.Filter{Distro: cmd.Distro, ...}` lines now read
      `repo.Filter{Distro: string(cmd.Distro), ...}` in
      `add.go:158, 237`, `delete.go:64`, `list.go:43`, `search.go:55`,
      `verify.go:138, 157`
- [x] update tests that set `Distro:` literal-style to use
      `Distro: DistroName("...")` (flag-parsed tests stay untouched, go-
      flags converts through reflection) — no changes needed; remaining
      `Distro: "..."` literals are on `repo.Package` / `repo.Filter`
      (string fields), and `AddCmd{Distro: "debian"}` works via
      untyped-constant assignment to named string type
- [x] run `make test` — every existing add/delete/publish/verify/info/
      list/search test must still pass; this is a pure refactor
- [x] run `make lint` — confirm no `unused-parameter` or `revive`
      regressions

### Task 4: Add shell wrappers and `completion` subcommand

**Files:**
- Create: `pkg/cli/completion/bash.sh`
- Create: `pkg/cli/completion/zsh.sh`
- Create: `pkg/cli/completion/fish.sh`
- Create: `pkg/cli/completion.go`
- Create: `pkg/cli/completion_test.go`
- Modify: `pkg/cli/root.go`

- [x] write `pkg/cli/completion/bash.sh`, `zsh.sh`, `fish.sh` using the
      wording in Technical Details; each ≤ 20 lines
- [x] create `pkg/cli/completion.go` with:
  - `//go:embed` directive on an `embed.FS` package variable
  - `completionScripts` map populated at `init()`; missing embedded file
    is a panic (build-time guarantee)
  - `CompletionCmd` struct with positional `Shell` (no `choice:` tag —
    that tag is silently ignored on positional args)
  - `Execute` validates the shell value manually; unsupported shell
    returns `ExitUsage` with a message listing valid choices
- [x] in `pkg/cli/root.go`:
  - add `cmdCompletion = "completion"` constant
  - add `Completion CompletionCmd` field on `Options` with `command:`
    tag and a help description showing the install pattern
  - leave it out of `needsPublic` and `needsConfig`
  - extend the two `cmd == cmdInit || cmd == cmdInfo` branches in
    `resolveRepoRoot` (lines 434 and 445) to also short-circuit
    `cmdCompletion` — completion never touches `repo.db`
  - add the `case cmdCompletion: return opts.Completion.Execute(ctx)`
    dispatch
- [x] write `completion_test.go`: each valid shell choice emits a
      non-empty script containing a shell-specific sentinel
      (`complete -F` for bash, `compdef` for zsh, `complete -c repokit`
      for fish); unsupported shell (`powershell`) returns `ExitUsage`
      and writes the error message to stderr; missing positional fails
      via go-flags `required:"yes"` validation
- [x] run `make test` — must pass before Task 5

### Task 5: Switch `.deb` positionals to `flags.Filename`

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/info.go`
- Modify: any `*_test.go` that constructs `Args.Debs` / `Args.Deb` directly
  (discover via `grep -n "Args.Debs\|Args.Deb\b" pkg/cli/*_test.go`)

- [x] in `AddCmd`, change `Args.Debs []string` to
      `Args.Debs []flags.Filename`
- [x] adjust the loop in `AddCmd.run` / `processOne` to cast each
      element to `string` before passing to `deb.Inspect` /
      `pool.Filename`
- [x] in `InfoCmd`, change the positional `.deb` field to
      `flags.Filename` and cast at the read site
- [x] update any tests that construct `Args.Debs` / `Args.Deb` directly
      to use `flags.Filename` literals — none found (grep returned only
      runtime call sites in `add.go` / `info.go`)
- [x] run `make test` — all add/info tests pass; existing integration
      tests still drive through `cli.Run` with `[]string` argv and are
      unaffected by the field type

### Task 6: End-to-end integration test for completion

**Files:**
- Modify: `pkg/cli/integration_test.go`

- [x] add `TestIntegration_Completion` that:
  - calls `t.Setenv("GO_FLAGS_COMPLETION", "1")`
  - writes a config fixture under `tmpState` with
    `distros: [debian, customer/acme]`
  - invokes `cli.Run` with argv `["--repo", tmpState, ""]` and a
    `bytes.Buffer` for stdout — the empty trailing token is go-flags'
    "complete here" marker (per
    `vendor/.../go-flags/completion.go:complete`)
  - asserts that every public subcommand name appears in the captured
    output: `init add delete list search publish verify info completion`
  - asserts return code is `ExitOK`
  - in a sub-`t.Run`, drives `--distro` value completion with argv
    `["--repo", tmpState, "add", "--distro", ""]`; asserts both
    `debian` and `customer/acme` appear in stdout
- [x] confirm `t.Setenv` cleanup works — subsequent tests must not see
      `GO_FLAGS_COMPLETION` set (guaranteed by `t.Setenv`'s `t.Cleanup`
      restore; the full `make test` run after the new case passes
      confirms no leak into sibling integration tests)
- [x] run `make test` — must pass before Task 7

### Task 7: Documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [x] add a short "Shell completion" section to `README.md` after the
      usage table with the three install one-liners; keep it ≤ 10
      lines of prose
- [x] add one bullet to `CLAUDE.md` `## Architecture` describing the
      `DistroName` type and `parser.CompletionHandler` wiring — enough
      context that a future contributor does not "fix" the type back
      to `string` or remove the handler
- [x] add one bullet to `CLAUDE.md` `## Gotchas`: completion runs with
      a custom `CompletionHandler` that writes to the `stdout` writer
      and returns; `DistroName.Complete` must remain silent on any
      error because stderr noise corrupts shell completion output
- [x] run `make test` and `make lint` — final sweep, all clean

### Task N-1: Verify acceptance criteria

- [x] verify `repokit completion bash`, `repokit completion zsh`,
      `repokit completion fish` each emit non-empty output
- [x] verify `repokit completion powershell` exits with `ExitUsage`
      and the stderr message lists the valid shells
- [x] verify all seven subcommands with `--distro` still reject
      malformed and unconfigured distros (no regression from the type
      change)
- [x] verify `make test` passes including the new integration test
- [x] verify `make lint` clean
- [x] verify coverage on new code ≥ 80%

### Task N: Update plan status

- [x] mark every checkbox `[x]` reflecting reality
- [x] move this file:
      `mkdir -p docs/plans/completed && mv docs/plans/20260513-shell-completion.md docs/plans/completed/`

## Post-Completion

*Manual verification — outside this plan's scope, performed by the operator.*

**Manual shell-level smoke test** (not automated; requires the shells
installed locally):

1. `make build` to produce `.bin/repokit`.
2. For each shell:
   - bash: `source <(./.bin/repokit completion bash)` then
     `./.bin/repokit <TAB>` lists subcommands; `./.bin/repokit add --<TAB>`
     lists `--distro`, `--suite`, `--component`, `--replace`.
   - zsh: write the script to `${fpath[1]}/_repokit`, reload `compinit`,
     repeat the TAB checks.
   - fish: write the script to `~/.config/fish/completions/repokit.fish`,
     open a new shell, repeat.
3. With a configured repo at `~/.repokit/config.yaml` containing
   `distros: [debian, customer/acme]`, confirm
   `./.bin/repokit add --distro <TAB>` lists both.
4. Confirm `./.bin/repokit add <TAB>` triggers file completion (`.deb`
   files in cwd; broader filenames in bash/zsh, which is acceptable).

**Known limitation accepted by this design:**

When `DistroName.Complete` encounters a malformed `config.yaml`, a missing
state dir, or an unreadable file, it returns no completions silently. The
user cannot distinguish "no distros configured" from "config error" via TAB
output. This is a deliberate trade-off — writing diagnostics to stderr
during completion corrupts the shell's TAB display. Operators who hit
silent-empty completion should run a non-completion command (e.g.
`repokit list`) which surfaces the underlying error.

**Out of scope for this plan:**

- Suite completion (depends on which distro is selected — harder to do
  correctly inside a TAB context). Revisit if users ask.
- Package-name completion for `delete` / `info` (would open `repo.db`
  on every TAB). Revisit if users ask.
- Auto-install of completion scripts. Each shell stores them differently;
  punt to the user, document the one-liners in README.
