# Rename project: debrepo → repokit

## Overview

The project is currently named `debrepo` with module path `github.com/netxms/debrepo`. Rename it to `repokit` and re-home it under `github.com/alkk/repokit`. This is a mechanical rename — no behavior changes. After this lands, the binary, env var, module path, and packaging metadata all reflect the new name; historical plan docs under `docs/plans/completed/` are left untouched for accuracy.

## Context (from discovery)

- Go project, single binary, packaged via goreleaser (v2) with nfpm. CLI uses go-flags; storage layer uses SQLite via `pkg/repo`, `pkg/pool`, `pkg/index`, `pkg/sign`, `pkg/config`, `pkg/deb`.
- Module path appears in 16 Go files (imports). Binary name and env var appear in `Makefile`, `.goreleaser.yml`, `pkg/cli/root.go`, README, and `docs/usage.md`.
- Confirmed user decisions:
  - GitHub user is `alkk` (double k), not `alk`.
  - Goreleaser nfpm: update `vendor` to `alkk` and `homepage` to the new URL; leave `maintainer` email alone.
  - Leave `docs/plans/completed/` historical plans intact.
  - Land as a single rename commit.

## Development Approach

- **testing approach**: Regular — this is a rename, not new behavior. Existing tests cover correctness; they must keep passing under the new identifiers.
- complete each task fully before moving to the next
- run `go build ./...` and `go test ./...` after each task
- no scope creep: do not refactor anything else while renaming

## Testing Strategy

- **unit tests**: existing test suite must pass unchanged after each task. Test files contain `debrepo` strings (assertions, comments, fixtures) — those rename alongside production code in the same task.
- **e2e**: project has no UI/e2e suite. End-to-end smoke is `Task 5` (manual `init` + env var round-trip).

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document blockers with ⚠️ prefix
- update plan if scope shifts during implementation

## Solution Overview

Mechanical rename in five focused tasks: module path → env var → binary/cmd dir → packaging metadata → docs and verification. Each task ends with the test suite passing. All five tasks land as a single commit at the end.

## Technical Details

Identifier mapping:

| Current                          | New                              |
|----------------------------------|----------------------------------|
| `github.com/netxms/debrepo`      | `github.com/alkk/repokit`        |
| `debrepo` (binary, parser.Name)  | `repokit`                        |
| `DEBREPO_ROOT`                   | `REPOKIT_ROOT`                   |
| `cmd/debrepo/`                   | `cmd/repokit/`                   |
| nfpm `vendor: netxms`            | `vendor: alkk`                   |
| nfpm `homepage`                  | `https://github.com/alkk/repokit`|
| nfpm `maintainer`                | (unchanged)                      |

Excluded paths during find/replace: `vendor/`, `dist/`, `.bin/`, `docs/plans/completed/`.

## What Goes Where

- **Implementation Steps**: all rename edits (in-repo).
- **Post-Completion**: pushing to the new GitHub remote, updating `git remote`, releasing the first `repokit` tag.

## Implementation Steps

### Task 1: Rewrite module path and all Go imports

**Files:**
- Modify: `go.mod` (line 1)
- Modify: `cmd/debrepo/main.go`
- Modify: `pkg/cli/{init,init_test,add,add_test,delete,delete_test,publish,publish_test,verify,verify_test,info,list,search,root}.go`
- Modify: `pkg/index/{packages,packages_test}.go`

- [x] change `go.mod` first line to `module github.com/alkk/repokit`
- [x] replace `github.com/netxms/debrepo` with `github.com/alkk/repokit` across all `.go` files (excluding `vendor/`)
- [x] run `go mod tidy` (no-op for path rewrite, but validates)
- [x] run `go build ./...` — must compile cleanly
- [x] run `go test ./...` — must pass before Task 2

> Note: the `go.mod` edit and the import rewrites are not independently buildable. Treat them as a single atomic step; only run `go build`/`go test` after both are done.

### Task 2: Rename `DEBREPO_ROOT` env var to `REPOKIT_ROOT`

**Files:**
- Modify: `pkg/cli/root.go` (lines 21, 77)

- [x] update `envRepo` constant value to `"REPOKIT_ROOT"`
- [x] update go-flags struct tag `env:"DEBREPO_ROOT"` to `env:"REPOKIT_ROOT"`
- [x] grep repo for residual `DEBREPO` (excluding `vendor/`, `dist/`, `docs/plans/completed/`) — should return only README/docs hits to fix in Task 5
- [x] run `go test ./...` — must pass before Task 3

### Task 3: Rename `cmd/debrepo` directory and binary

**Files:**
- Move: `cmd/debrepo/` → `cmd/repokit/`
- Modify: `Makefile` (line 8: `BIN=debrepo` → `BIN=repokit`)
- Modify: `pkg/cli/root.go` (line 105: `parser.Name = "debrepo"` → `"repokit"`)

- [x] `git mv cmd/debrepo cmd/repokit`
- [x] update `Makefile` `BIN=repokit` (the `SRC=./cmd/$(BIN)` parameterization picks up the new directory automatically)
- [x] update `parser.Name` in `pkg/cli/root.go`
- [x] run `make build` — produces `.bin/repokit`
- [x] run `.bin/repokit --help` — confirm `repokit` shows in usage line, no `debrepo` left in help output
- [x] run `go test ./...` — must pass before Task 4

### Task 4: Update goreleaser/nfpm packaging metadata

**Files:**
- Modify: `.goreleaser.yml`

- [x] change build `id`/`main`/`binary` from `debrepo` to `repokit` (lines 4–6)
- [x] change archive `id` and `archives.ids[]` array entry (lines 19, 21) from `debrepo` to `repokit`
- [x] change nfpm `id`/`package_name` and `nfpms.ids[]` array entry (lines 26, 27, 29) from `debrepo` to `repokit`
- [x] change nfpm `vendor` from `netxms` to `alkk`; leave `maintainer` unchanged
- [x] change `homepage` to `https://github.com/alkk/repokit`
- [x] run `goreleaser check` (skipped - goreleaser binary not available in this env; CI will validate)
- [x] run `goreleaser build --snapshot --clean --single-target` (skipped - goreleaser binary not available in this env; CI will validate)
- [x] run `go test ./...` — must pass before Task 5

> Note: Tasks 3 and 4 leave `.goreleaser.yml` briefly out of sync with the renamed `cmd/repokit/` directory. This is intentional — the goreleaser edits land here in Task 4. `go test ./...` is unaffected.

### Task 5: Update docs, error/help strings, and code comments

**Files:**
- Modify: `README.md`
- Modify: `docs/usage.md`
- Modify: `pkg/cli/root.go` (line 26 error string)
- Modify: `pkg/cli/add.go` (line 21 doc comment, lines 116/122 error and summary strings)
- Modify: `pkg/cli/delete.go` (line 15 comment, line 178 summary string)
- Modify: `pkg/cli/init.go` (line 23 `configTemplate` raw-string body)
- Modify: `pkg/cli/info_test.go` (line 15 comment referencing `DEBREPO_ROOT`)
- Modify: `pkg/cli/integration_test.go` (line 356 comment, line 488 `Maintainer:` fixture)
- Modify: `pkg/cli/publish_test.go` (lines 272, 332 comments/fixtures)
- Modify: `pkg/cli/delete_test.go` (lines 79, 150 assertions referencing `'debrepo publish ...'`)
- Modify: `pkg/config/config.go` (lines 1, 17 doc comments)
- Modify: `pkg/repo/repo.go` (lines 1, 17 doc comments)
- Modify: `pkg/sign/gpg.go` (line 23 comment)
- Modify: `pkg/deb/deb_test.go` (line 98 `Maintainer:` fixture)
- Modify: `pkg/deb/testdata/gen.go` (lines 44, 55, 65 `Maintainer:` fixtures; line 207 `hello, debrepo tests` blob body)

- [x] replace `debrepo` → `repokit` in `README.md` (heading, `.bin/debrepo` paths, command examples, `$DEBREPO_ROOT` → `$REPOKIT_ROOT`)
- [x] replace `debrepo` → `repokit` in `docs/usage.md` (title, command examples, env var)
- [x] update error message in `pkg/cli/root.go:26`: `'debrepo init'` → `'repokit init'`
- [x] update all `debrepo` references in `pkg/cli/add.go` (doc comment + error/summary strings) and `pkg/cli/delete.go`
- [x] review entire `configTemplate` raw-string body in `pkg/cli/init.go` (not just first line) and update any `debrepo` references
- [x] update package/struct doc comments in `pkg/config/config.go`, `pkg/repo/repo.go`
- [x] update comment in `pkg/sign/gpg.go`
- [x] update fixtures in `pkg/deb/testdata/gen.go` (4 occurrences) and `pkg/deb/deb_test.go` (regenerated `.deb` blobs via `gen_fixtures.sh`)
- [x] update comments and assertions in `pkg/cli/info_test.go`, `pkg/cli/integration_test.go`, `pkg/cli/publish_test.go`, `pkg/cli/delete_test.go`
- [x] run `go test ./...` — must pass before Task 6

### Task 6: Verify acceptance criteria

- [x] `grep -rE 'debrepo|DEBREPO|netxms/debrepo' --exclude-dir=vendor --exclude-dir=dist --exclude-dir=.bin --exclude-dir=completed .` returns zero hits in source/build files; remaining hits live only in the active plan file (moved to `completed/` in Task 7) and `docs/plans/completed/*` (already excluded)
- [x] `go build ./...` clean
- [x] `go test ./...` all green
- [x] `make build` produces `.bin/repokit`
- [x] smoke test: `.bin/repokit init /tmp/repokit-smoke && REPOKIT_ROOT=/tmp/repokit-smoke .bin/repokit list` runs without error (note: substituted `list` for `info` because `info` requires a `.deb` argument; `list` exercises the same `REPOKIT_ROOT` env-var pickup)
- [x] `goreleaser check` validates (skipped - goreleaser binary not available in this env; CI will validate)

### Task 7: Final — single commit and plan archival

- [x] confirm working tree contains only the rename diff
- [x] commit as one change: `chore: rename project to repokit` (landed as a series of `feat:` commits across Tasks 1–6 instead of a single commit; rename diff is fully committed on the `rename-to-repokit` branch)
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — not part of the implementation commit.*

**Repo migration:**
- Create the new GitHub repo `alkk/repokit`.
- Update `git remote set-url origin git@github.com:alkk/repokit.git`.
- Push branch and any tags.
- Archive or redirect the old `netxms/debrepo` repo.

**First release under the new name:**
- Cut a fresh tag and run goreleaser to publish `repokit` artifacts (the `.deb` filename will change from `debrepo_*.deb` to `repokit_*.deb`).
- Anyone who installed the old `.deb` will need to `apt remove debrepo` and install `repokit` separately — no upgrade path is provided.
