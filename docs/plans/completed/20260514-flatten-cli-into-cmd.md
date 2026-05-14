# Flatten pkg/cli into cmd/repokit

## Overview

`pkg/cli/` is a 2,700-line "library" with exactly one consumer:
`cmd/repokit/main.go`. The `pkg/` placement implies an importable
library API, but nothing outside the module imports it and nothing
inside does either. The package boundary buys no encapsulation
(every test is white-box `package cli`) and forces a thin 19-line
shim in `main.go` whose only job is to call `cli.Run`.

This refactor moves every `pkg/cli/*.go` file into `cmd/repokit/`
as `package main`, deletes the shim, and unexports the symbols that
were only public because of the package split (`Run`, `ExitOK`,
`ExitError`, `ExitUsage`, `ErrRepoNotFound`).

## Context (from discovery)

- Source files to move: 12 `*.go` + 13 `*_test.go` under `pkg/cli/`
  (25 files, ~2,700 LOC source + ~5,500 LOC tests)
- Embedded asset directory: `pkg/cli/completion/{bash,zsh,fish}.sh`
  referenced by `//go:embed completion/...` in `completion.go`.
  `//go:embed` paths resolve relative to the source file's
  directory, so the directive itself doesn't change after the move.
- Only external consumer: `cmd/repokit/main.go:18` calls
  `cli.Run(argv, stdout, stderr, revision)`
- Build tooling already binary-aware: `Makefile` line 9
  `SRC=./cmd/$(BIN)`, `.goreleaser.yml` line 5 `main: ./cmd/repokit`
  — no changes needed there
- In-package call sites are extensive (~200): every `*_test.go`
  calls `Run(...)` and asserts against `ExitOK` / `ExitError` /
  `ExitUsage`; `root_test.go` references `ErrRepoNotFound`. The
  rename is mechanical but its blast radius is much larger than
  the five definition sites.
- Two test files have **cwd-dependent relative paths**:
  - `pkg/cli/add_test.go:78` — `filepath.Join("..", "deb",
    "testdata", name)` resolves to `pkg/deb/testdata/...` when cwd
    is `pkg/cli`. After the move cwd is `cmd/repokit` and
    `cmd/repokit/../deb` does not exist. Must become
    `filepath.Join("..", "..", "pkg", "deb", "testdata", name)`.
  - `pkg/cli/integration_realpkg_test.go:21` —
    `filepath.Join("..", "..", "test-packages", rel)` resolves to
    repo-root/test-packages from either `pkg/cli` or `cmd/repokit`
    by coincidence (both are two dirs deep). Path survives; only
    the comment on line 18 needs updating.
  - `pkg/cli/integration_test.go` — all `filepath.Join` calls
    descend from a `t.TempDir()` root, not cwd. Safe.
- Stale comments to update post-move:
  - `add_test.go:75` ("cwd == pkg/cli" → "cwd == cmd/repokit")
  - `integration_realpkg_test.go:18` (same)
  - `integration_test.go:407` (`cli.Run` → `run`)
  - `pkg/config/config.go:102` (`` `pkg/cli` `` →
    `` `cmd/repokit` `` — comment only, no behaviour change)
- CLAUDE.md has 9 references to update: 6 paths
  (`pkg/cli/...` → `cmd/repokit/...`) and 4 prose qualifiers
  (`cli.DistroName`, `cli.Run` × 3). Use a grep gate after the
  edit instead of pinning line numbers.
- Completed plans under `docs/plans/completed/` reference
  `pkg/cli/` — historical, leave untouched.

## Development Approach

- **Testing approach**: Regular — this is a mechanical rename, not
  new behaviour. Existing tests (`integration_test.go`,
  `integration_realpkg_test.go`, every `*_test.go` per subcommand)
  already cover the surface. They must keep passing after the move
  with zero logic changes inside them.
- Single atomic task — the change only compiles as a whole. A
  partial state (some files `package cli`, some `package main`)
  doesn't build, so splitting buys nothing.
- Run `make test` and `make lint` before declaring done.
- Update plan file (this document) only if scope expands during
  the move (e.g., an undiscovered consumer of `cli.*` shows up).

## Testing Strategy

- **Unit tests**: every existing `*_test.go` file moves with its
  source file and keeps passing. No new tests written — refactor
  preserves behaviour.
- **Integration tests**: `integration_test.go` and
  `integration_realpkg_test.go` exercise the full
  `init → add → publish → verify` flow through the public entry
  point. After the move that entry point is the unexported `run`
  function in `package main`; tests must call `run(...)` directly
  instead of `cli.Run(...)` (or `Run` if the file is in the same
  package, which it now is).
- **No e2e suite** in this project — the integration tests are the
  highest-level coverage.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

Three mechanical edits per source file, executed once across the
24 files:

1. Replace `package cli` → `package main` at line 1.
2. Drop the export prefix on identifiers that were only exported
   to cross the `pkg/cli` → `cmd/repokit/main` boundary:
   - `cli.Run` → `run` (called from `main()` in `main.go`)
   - `cli.ExitOK`, `cli.ExitError`, `cli.ExitUsage` → `exitOK`,
     `exitError`, `exitUsage` (or keep uppercase as unexported
     package-level consts — Go convention is unexported = lowercase,
     so we lowercase)
   - `cli.ErrRepoNotFound` → `errRepoNotFound`
3. For files referencing those symbols inside the package, the
   short-name form (`Run`, `ExitOK`, etc. without `cli.` qualifier)
   is already what's used since they live in the same package.
   But there are ~200 such in-package call sites (every `*_test.go`
   exercises `Run(...)` and asserts on `ExitOK`/`ExitError`/
   `ExitUsage`). A blanket lowercase rename via grep + sed (or
   `gofmt -r`) handles them. Don't be surprised by a large diff —
   the change is still mechanical.

Then merge `cmd/repokit/main.go` with the moved files:
- existing `var revision = "unknown"` stays — it's a package-level
  var in `main` and the `-ldflags "-X main.revision=..."` from
  Makefile/goreleaser still binds correctly.
- existing `main()` function becomes the only `main()` in the
  package; its body changes from `os.Exit(run(os.Args[1:], ...))`
  with `run` delegating to `cli.Run`, to `os.Exit(run(os.Args[1:],
  os.Stdout, os.Stderr, revision))` where `run` is the entry
  function from what used to be `root.go` (renamed from `Run`).
- the shim `run` function (lines 17-19 of current `main.go`)
  disappears — its only purpose was to wrap `cli.Run`.

The 24 moved files keep their names. `cmd/repokit/completion/`
subdirectory is created and the three `.sh` files move there
verbatim — `//go:embed completion/bash.sh` etc. resolve relative to
the embedding source file, so the embed directive itself doesn't
change.

## Technical Details

### Identifier rename table

| Before (`pkg/cli` exports)   | After (`package main`)   |
|------------------------------|--------------------------|
| `cli.Run`                    | `run`                    |
| `cli.ExitOK`                 | `exitOK`                 |
| `cli.ExitError`              | `exitError`              |
| `cli.ExitUsage`              | `exitUsage`              |
| `cli.ErrRepoNotFound`        | `errRepoNotFound`        |

Inside `pkg/cli`, these are already referenced without the `cli.`
prefix, so the in-package call sites change only the first letter
(e.g., `ExitOK` → `exitOK`). All occurrences within the moved
files must be updated; `grep -nE '\b(Run|ExitOK|ExitError|ExitUsage|ErrRepoNotFound)\b'`
on the moved files will surface them.

Names that look exported but stay as-is (they aren't part of the
`cli.*` boundary — they're command structs and types used by
go-flags reflection or by other internal callers):
- `Options`, `AddCmd`, `DeleteCmd`, `InitCmd`, … — go-flags reads
  struct tags via reflection; lowercasing the type names is fine
  in `package main`, but it ripples through every test. Out of
  scope for this refactor — the goal is removing the *package*
  boundary, not renaming every internal type. Leave them
  capitalised; Go permits exported names in `package main`.
- `Context`, `DistroName`, `ErrSuiteNotFound` (if any) — same
  rationale.

### File move list

Source files (12):
- `add.go`, `completion.go`, `delete.go`, `distro_name.go`,
  `info.go`, `init.go`, `list.go`, `publish.go`, `root.go`,
  `search.go`, `verify.go`

Test files (12):
- `add_test.go`, `completion_test.go`, `delete_test.go`,
  `distro_name_test.go`, `info_test.go`, `init_test.go`,
  `integration_realpkg_test.go`, `integration_test.go`,
  `list_test.go`, `publish_test.go`, `root_test.go`,
  `search_test.go`, `verify_test.go`

Embedded assets (3):
- `completion/bash.sh`, `completion/zsh.sh`, `completion/fish.sh`

### What stays unchanged

- `Context` struct, `needsPublic`/`needsConfig` sets, dispatch
  `switch`, `validateScopeName`/`validateDistroScope` split, every
  subcommand handler, `DistroName.Complete`, the custom
  `parser.CompletionHandler` wiring — pure behaviour preservation.
- `Makefile` (`SRC=./cmd/$(BIN)` already correct).
- `.goreleaser.yml` (`main: ./cmd/repokit` already correct).
- All `pkg/<other>/` packages.
- `cmd/repokit/main.go`'s `var revision = "unknown"` declaration.

## What Goes Where

- **Implementation Steps** (`[x]` checkboxes): everything below.
- **Post-Completion**: no external systems involved — this is a
  source-tree relocation, no consumers downstream.

## Implementation Steps

### Task 1: Move pkg/cli into cmd/repokit as package main

**Files:**
- Move (25 Go files): every `pkg/cli/*.go` → `cmd/repokit/`. Use
  `git mv pkg/cli/*.go cmd/repokit/` (the glob picks up both
  source and `_test.go` files; verify with `ls pkg/cli/*.go`
  beforehand).
- Move: `pkg/cli/completion/` → `cmd/repokit/completion/` (the
  three `bash.sh` / `zsh.sh` / `fish.sh` embedded scripts)
- Modify: `cmd/repokit/main.go` (merge with moved `root.go`, drop shim)
- Modify: `pkg/cli/add_test.go` → `cmd/repokit/add_test.go` (fix
  relative testdata path + comment)
- Modify: `cmd/repokit/integration_realpkg_test.go` (comment only)
- Modify: `cmd/repokit/integration_test.go` (one comment at line ~407)
- Modify: `pkg/config/config.go` (one comment at line 102)
- Modify: `CLAUDE.md` (paths + prose qualifiers)
- Delete: `pkg/cli/` directory (empty after move)

Steps:

- [x] `ls pkg/cli/*.go` to confirm the file set, then
  `git mv pkg/cli/*.go cmd/repokit/`
- [x] `git mv pkg/cli/completion cmd/repokit/completion`
- [x] In every moved `.go` file, change `package cli` → `package main`
  (covers both source and test files in one sweep)
- [x] In `cmd/repokit/root.go`, rename the five exports:
  `Run` → `run`, `ExitOK` → `exitOK`, `ExitError` → `exitError`,
  `ExitUsage` → `exitUsage`, `ErrRepoNotFound` → `errRepoNotFound`
- [x] Rename every in-package call site (largely in test files):
  `grep -nE '\b(Run|ExitOK|ExitError|ExitUsage|ErrRepoNotFound)\b' cmd/repokit/*.go`
  and apply the same lowercase mapping. `gofmt -r 'Run -> run'`
  (etc.) on `cmd/repokit/` is the safest mechanical option. Be
  careful: `Run` is a common identifier — restrict to whole-word
  matches on the *call expression* side, not unrelated struct
  fields. Spot-check after.
- [x] Fix the cwd-relative testdata path in
  `cmd/repokit/add_test.go:78`:
  `filepath.Join("..", "deb", "testdata", name)` →
  `filepath.Join("..", "..", "pkg", "deb", "testdata", name)`.
  Update the comment on line 75 from `cwd == pkg/cli` to
  `cwd == cmd/repokit`.
- [x] Update the comment on
  `cmd/repokit/integration_realpkg_test.go:18`:
  `cwd == pkg/cli` → `cwd == cmd/repokit`. (The path itself
  works unchanged — `../../test-packages` lands at repo root from
  either origin.)
- [x] Update the comment in `cmd/repokit/integration_test.go`
  (around line 407): `cli.Run` → `run`.
- [x] Update the comment in `pkg/config/config.go:102`:
  `` `pkg/cli` `` → `` `cmd/repokit` ``.
- [x] Merge `cmd/repokit/main.go`: delete the existing `run` shim
  function; the `main()` body becomes
  `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, revision))`
  calling the `run` symbol now defined in the same package
  (formerly `root.go`'s `Run`). Drop the
  `github.com/alkk/repokit/pkg/cli` import. Keep
  `var revision = "unknown"`.
- [x] Confirm `pkg/cli/` is empty, then `rmdir pkg/cli`
- [x] Update `CLAUDE.md`: rewrite every `pkg/cli` path to
  `cmd/repokit`, and every `cli.X` qualifier (`cli.DistroName`,
  `cli.Run` × 3) to its unqualified `package main` form
  (`DistroName`, `run`). Final gate:
  `grep -nE 'pkg/cli|cli\.' CLAUDE.md` must be empty.
- [x] Final cross-tree check:
  `git grep -nE 'pkg/cli|\bcli\.' -- ':!docs/plans/completed/' ':!docs/plans/20260514-flatten-cli-into-cmd.md'`
  must be empty.
- [x] `go build ./...` — must compile
- [x] `make test` — must pass with race detector (the new testdata
  path in `add_test.go` is the only behaviour-affecting edit; if
  it's wrong, `add` / `delete` / `publish` tests fail with "no
  such file")
- [x] `make lint` — must be clean
- [x] `make fmt` — apply and commit any formatting drift

### Task 2: Verify acceptance

- [x] `make build` produces `.bin/repokit` and `.bin/repokit.<branch>` as before
- [x] `.bin/repokit --help` shows revision in long description (binary has no `--version` flag; revision is exposed via `parser.LongDescription` in `root.go`, which proves `-ldflags -X main.revision=...` still binds)
- [x] `.bin/repokit completion bash | head -5` emits the embedded bash wrapper (proves `//go:embed completion/bash.sh` resolved relative to the moved file)
- [x] `make race` clean
- [x] `git ls-files pkg/cli` returns nothing (no orphaned files)
- [x] `git grep -n 'pkg/cli' -- ':!docs/plans/completed/' ':!docs/plans/20260514-flatten-cli-into-cmd.md'` returns nothing (CLAUDE.md and other tree updated; this plan and completed plans preserved as history)

### Task 3: [Final] Move plan to completed

- [x] `mv docs/plans/20260514-flatten-cli-into-cmd.md docs/plans/completed/`

## Post-Completion

*None.* This refactor is fully internal. No consumers downstream,
no deployment changes, no documentation outside CLAUDE.md to update.
The release artefact path (`.bin/repokit`) and the import path
(`github.com/alkk/repokit/cmd/repokit`) are unchanged.
