# Clean up `<suite>.new` after a failed publish

## Overview

When `debrepo publish` fails partway through, the staging directory
`<root>/<distro>/dists/<suite>.new` is left on disk. The next publish run
eventually wipes it via `cleanStaging`, but until then a user inspecting the
tree sees a stale `.new` sibling and gets confused — the most reproducible
trigger today is a suite that contains only `arch=all` packages, which
`writeIndexes` rejects *after* `MkdirAllSync(newPath)` has already created
the staging dir.

**Fix.** Two coordinated changes in `pkg/cli/publish.go`:

1. **Pre-flight the suite enumeration** before any filesystem work.
   `r.EnumerateForSuite` plus the "must have a concrete arch" guard moves
   from inside `writeIndexes` to the top of `publishScopeLocked`. If the
   guard trips, we return early — `.new` was never created.
2. **Defer-on-error cleanup.** Right after `MkdirAllSync(newPath)` succeeds,
   install a `defer` that removes `newPath` when the function returns a
   non-nil error. On the success path, `swapDists` has already renamed
   `.new` → live, so the deferred `RemoveAll` is a no-op anyway, but
   gating on `err != nil` keeps the intent explicit.

`cleanStaging` stays — it remains the safety net for crashes (process
killed, OS panic) where defers don't run.

## Context (from discovery)

- Single file: `pkg/cli/publish.go`. `publishScopeLocked` (lines 172–207)
  is the function that creates and promotes the staging tree.
- Existing arch=all guard lives in `writeIndexes` at lines 264–269 — it
  fires *after* `MkdirAllSync(newPath)` at line 182. That ordering is the
  immediate cause of the leftover `.new`.
- `cleanStaging` (line 214) wipes leftover `.new` and `.old` at the start
  of every publish — keep, it covers crash-not-error paths.
- Existing test `TestPublishRejectsArchAllOnlySuite`
  (`pkg/cli/publish_test.go:166`) already exercises the arch=all-only path
  but only asserts `bookworm` (live) does not exist; it does not assert
  `bookworm.new` is absent — that gap is exactly the bug.
- `swapDists` (line 388) renames `.new` → live on success, so a deferred
  `RemoveAll(newPath)` is harmless on the happy path.
- `EnumerateForSuite` lives at `pkg/repo/queries.go:202`,
  `index.ConcreteArches` at `pkg/index/release.go:77` — both already
  imported by `publish.go`.

## Development Approach

- **testing approach**: Regular (code first, then tests) — small refactor
  with two narrow behavior changes; tests follow in the same task.
- complete each task fully before moving to the next.
- make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes
  in that task.
- **CRITICAL: all tests must pass before starting next task** — no
  exceptions.
- run `make test` (or `go test ./...`) after each task.
- no schema changes, no public API changes — purely an internal cleanup
  of `publishScopeLocked`.

## Testing Strategy

- **unit tests**: required for every task. New assertions go in
  `pkg/cli/publish_test.go` next to the existing publish tests.
- No e2e suite in this project — `pkg/cli/integration_test.go` already
  covers add → publish → verify; only spot-check it if a regression
  surfaces.

## Progress Tracking

- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix.
- document issues/blockers with ⚠️ prefix.
- update plan if implementation deviates from original scope.

## Solution Overview

`publishScopeLocked` reorganizes into three phases:

1. **Read + validate** (`EnumerateForSuite` + concrete-arch guard) — pure
   reads, no filesystem mutations. Failures here return before any staging
   directory exists.
2. **Stage** (`cleanStaging` → `MkdirAllSync` → install error defer →
   `writeIndexes` → `writeReleaseFile` → `signer.Sign` → `FsyncTreeDirs`).
   Any error from this phase trips the defer, which removes `<suite>.new`.
3. **Promote** (`swapDists`). After `swapDists` returns, `.new` no longer
   exists at that path; defer is a no-op.

`writeIndexes` accepts the already-enumerated `components` and concrete
`arches` slices instead of fetching them itself. The redundant arch-only
guard inside `writeIndexes` is removed (the caller has already enforced
it).

## Technical Details

- **Function signature change**: `publishScopeLocked` gains a named return
  `(err error)` so the deferred cleanup can read the post-return value.
- **`writeIndexes` signature change** (internal): now takes
  `components []string`, `concrete []string` instead of fetching them.
  No callers outside `publishScopeLocked`.
- **Defer pattern**:
  ```go
  defer func() {
      if err != nil {
          _ = os.RemoveAll(newPath)
      }
  }()
  ```
  Placed immediately after `MkdirAllSync(newPath)` succeeds so it does not
  fire if creation itself failed (nothing to remove in that case).

## What Goes Where

- **Implementation Steps**: code changes in `pkg/cli/publish.go` and test
  additions in `pkg/cli/publish_test.go`.
- **Post-Completion**: nothing — this is a self-contained internal fix
  with no external consumers.

## Implementation Steps

### Task 1: Hoist suite enumeration + arch guard out of `writeIndexes`

**Files:**
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/publish_test.go`

- [x] in `publishScopeLocked`, call `r.EnumerateForSuite` and compute
      `index.ConcreteArches` *before* `cleanStaging` / `MkdirAllSync`;
      return the existing `arch=all`-only error from there
- [x] change `writeIndexes` signature to accept the precomputed
      `components` and `concrete` slices; drop its own enumeration call
      and the redundant arch-only guard. The raw `architectures` slice
      from `EnumerateForSuite` is no longer needed past the guard — only
      `concrete` propagates into `writeIndexes`
- [x] update the single call site in `publishScopeLocked` to pass them in
- [x] extend `TestPublishRejectsArchAllOnlySuite` to additionally assert
      both `<suite>.new` AND `<suite>.old` are absent:
      `assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm.new"))`
      and the `.old` equivalent (symmetry with `TestPublishEmptySuite`)
- [x] run `go test ./pkg/cli/...` — must pass before next task

### Task 2: Defer-on-error cleanup of `<suite>.new`

**Files:**
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/publish_test.go`

- [x] change `publishScopeLocked` to use a named return `(err error)`.
      The current `return swapDists(...)` (publish.go:206) needs to keep
      the assignment-then-return form so the defer reads the
      post-`swapDists` value. On the happy path `swapDists` has already
      renamed `.new` → live, so the deferred `RemoveAll(newPath)` is a
      no-op; on a `swapDists` partial failure (live → `.old` succeeded
      but `.new` → live did not), `.new` still exists and the defer
      cleans it — exactly the case we want
- [x] immediately after `pool.MkdirAllSync(newPath)` succeeds, install a
      defer that calls `os.RemoveAll(newPath)` when `err != nil`
- [x] add `failingSigner` stub in `publish_test.go` — a 5-line
      `sign.Signer` whose `Sign` returns `errors.New("boom")`
- [x] add `TestPublishRemovesStagingOnFailure` that calls `publishScope`
      directly (same `cli` package — `pickSigner` cannot be subbed via
      the public `Run([]string{...})` seam, so the test bypasses it)
      with the `failingSigner` and a populated suite that has a concrete
      arch, so the function reaches `signer.Sign` after `.new` exists
- [x] in that test, assert publish returns a non-nil error AND
      `<suite>.new` does not exist on disk afterwards
- [x] verify the success path still works:
      `TestPublishAtomicSwapReplacesLive` and friends continue to pass
- [x] run `go test ./pkg/cli/...` — must pass before next task

### Task 3: Verify acceptance criteria

- [x] re-read `publishScopeLocked` end-to-end and confirm the three-phase
      ordering (read+validate → stage → promote) holds
- [x] confirm `cleanStaging` is still invoked at the start of every run
      (crash-recovery path unchanged)
- [x] confirm no caller outside `publishScopeLocked` references the new
      `writeIndexes` signature
- [x] run full test suite: `go test ./...`
- [x] run `make lint` if the project ships a Makefile target for it
      (shadow regression fixed in df16ef2; `make lint` reports 0 issues)

### Task 4: [Final] Update documentation

- [x] no README/CLAUDE.md changes needed (verified)
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

*Nothing — internal fix, no external consumers, no manual verification
beyond the standard test suite.*
