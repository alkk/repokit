# Defer pool removal from `delete` and split orphan GC into its own subcommand

## Overview

Today `repokit delete` removes DB rows **and** unlinks the corresponding pool
files in the same writer-locked transaction. Between `delete` and the next
`publish`, `dists/*/Packages*` indexes still reference filenames that are
already gone from the pool, so apt clients hitting the repo in that window
get `404 Not Found` on the affected `.deb`s.

This plan flips the model so `delete` only touches the database. Pool files
become orphans (a state already understood by `verify`) and are reaped by a
new, separate `repokit gc` subcommand. The window between `delete` and
`publish` now leaves the on-disk repo internally consistent: indexes still
point at files that still exist, just at versions the operator has decided to
retire. The retirement becomes user-visible only at `publish`.

Companion change: the existing `verify --gc` flag is removed. `verify`
becomes strictly read-only / reporting; orphan removal lives entirely under
the new `gc` command.

Brainstorm decisions that anchor this plan:

1. Pool removal happens **only via explicit `repokit gc`** — not auto-pruned
   by `publish`, no opt-in flag.
2. `verify --gc` is dropped (pre-1.0; clean break is preferable to keeping a
   deprecated alias).
3. New command name is `gc` (matches the existing flag terminology).
4. `gc` is **orphan-only** — no `--name` / `--sha256` filters. Surgical
   removal is what `delete` is for.
5. `gc` uses **existence check only**, no hashing. Hashing belongs to
   `verify`; `gc` only needs the set of pool paths known to the DB.

## Context (from discovery)

- `cmd/repokit/delete.go` — `run` / `runDelete` / `runDryRun` / `report`.
  The pool-unlink loop lives inside `WithWriterLock`. The block comment
  above it motivates the lock around the DB+pool pair — that motivation
  evaporates once the pool half is removed.
- `cmd/repokit/verify.go` — `VerifyCmd` with `GC bool`, `verifyAndGC`,
  `removeOrphans`, `matchOrphanDistro`, `scanOrphans`,
  `scanOrphansAcrossDistros`, plus `scanOrphansForScope` (a `*VerifyCmd`
  method that dispatches between the scoped and unscoped paths) and
  `scopePoolDir` / `poolDirName`. The whole orphan-walk machinery
  (everything except the `hashAndIndex` step) is what `gc` needs to reuse.
- `pkg/repo/queries.go` — `Repo.Delete(ctx, *sql.Tx, Filter)` requires a
  surrounding transaction (it's not a one-shot). `DeleteConn` takes a
  writer-locked `*sql.Conn`. Either path requires us to manage the
  transaction; keeping the existing `WithWriterLock` + `DeleteConn` is
  the lowest-churn replacement and is what Task 4 picks.
- `cmd/repokit/main.go` — subcommand-name constants (`cmdAdd`, `cmdDelete`,
  `cmdPublish`, `cmdVerify`, …). `Options` registers each command via
  go-flags tags. `needsPublic` and `needsConfig` gate `Context.PublicDir`
  and `Context.Config` resolution by command name; `Context.PublicDir` is
  populated for `add/delete/publish/verify`.
- `pkg/pool/pool.go` — `Delete`, `PruneEmptyParents`, `SafeJoin`,
  `DiskPath`. All reused unchanged by `gc`.
- `cmd/repokit/completion/hook.{bash,fish,zsh}` — go-flags emits subcommand
  names dynamically via `GO_FLAGS_COMPLETION=verbose`, so the new `gc`
  command will surface in completion without script edits.
- `cmd/repokit/integration_test.go` and `integration_realpkg_test.go` —
  end-to-end tests that chain `add → delete → publish → verify`. Likely
  assert pool-file absence after `delete`; need an audit pass.
- `docs/plans/completed/20260515-prune-empty-pool-dirs.md` — prior plan in
  this area; format and style template followed here.
- `CLAUDE.md` — "Architecture" and "Gotchas" sections reference
  `verify --gc` and the delete flow's pool-unlink semantics; both need
  updating.

## Development Approach

- **testing approach**: Regular (code first, then tests within the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional — they are a required part of the checklist
  - write unit tests for new functions
  - update existing test cases when behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- this is a pre-1.0 project; `verify --gc` is removed without a deprecation
  alias

## Testing Strategy

- **unit tests**: required for every task. `cmd/repokit/gc_test.go` covers
  the new command end-to-end (DB query → walk → remove). `verify_test.go`
  and `delete_test.go` are updated to match the new contract.
- **integration tests**: `integration_test.go` /
  `integration_realpkg_test.go` need an audit to swap `delete`-removes-pool
  assertions for `delete`-leaves-orphan + `gc`-removes-orphan.
- **race tests**: `gc` keeps the same writer-lock semantics as today's
  `verify --gc`, so `make race` should remain green; if a `verify --gc` race
  test exists, port it to `gc`.
- run `make test` (race + coverage) and `make lint` after each task.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

Three-part change, ordered to keep each task independently green:

1. **Extract shared orphan helpers** from `verify.go` into a private
   `cmd/repokit/orphans.go`. No behavior change yet; `verify --gc` keeps
   working, all existing tests pass. This isolates the moving-around
   churn from the semantic changes.
2. **Add `gc` subcommand** in `cmd/repokit/gc.go`, wire it into
   `main.go` (constant + `Options` field + dispatch case + `needsPublic` +
   `needsConfig`). Build `known` via a single DB query (`SELECT DISTINCT
   distro, filename FROM packages [WHERE …]`), call into the extracted
   helpers from step 1.
3. **Remove `--gc` from `verify`** and simplify the surrounding code paths;
   refactor `delete` to be DB-only and update its tests.

Then docs (`CLAUDE.md`, `README.md` if applicable) and integration-test
audit close it out.

Approach trade-off considered (and rejected): adding a `gc bool`-style
parameter back to the shared walk so both commands call the *same* function.
Rejected because `verify` and `gc` build `known` differently anyway
(hash-as-you-go vs single bulk query) — the only thing genuinely worth
sharing is the walk + remove + prune. Keeping the helper a thin slab of
walk logic minimizes coupling.

## Technical Details

**New shared helpers** (`cmd/repokit/orphans.go`, package main, unexported):

- `scanPoolForOrphans(publicDir, distro, suite string, known map[string]struct{}) ([]string, error)`
  — generalises today's `scanOrphans`. Walks
  `<publicDir>/<distro>/pool/[suite]/...`, skips `.tmp`/`.bak`, returns
  disk-relative paths (`<distro>/pool/...`) not present in `known`.
- `scanPoolForOrphansAcrossDistros(publicDir string, distros []string, known map[string]struct{}) ([]string, error)`
  — generalises today's `scanOrphansAcrossDistros`. Iterates configured
  distros and concatenates.
- `removeOrphans(publicDir string, orphans, distros []string, stderr io.Writer) error`
  — verbatim move from `verify.go`. Deletes each orphan and prunes empty
  parents (longest-prefix distro match guards the prune stop boundary).
- `matchOrphanDistro(p string, distros []string) string` — verbatim move.
- `scanOrphansForScope` (currently a `*VerifyCmd` method dispatching
  between the scoped and unscoped helpers) — after Task 3 it has exactly
  one caller, so inline it into `verifyOnly` rather than moving it.

**New command** (`cmd/repokit/gc.go`):

```go
type GcCmd struct {
    Distro DistroName `long:"distro" description:"limit gc to one distribution (plain like 'debian' or namespaced like 'customer/acme')"`
    Suite  string     `long:"suite" description:"limit gc to one suite (requires --distro)"`
    DryRun bool       `long:"dry-run" description:"print orphans that would be removed without making changes"`
}
```

Flow:
1. Validate flags: `validateDistroScope` + `validateScopeName(fieldSuite, …)`;
   `--suite` requires `--distro`; if `--distro` given,
   `requireConfiguredDistro`; else `requireConfiguredDistros`.
2. Open repo; pull `cfg.Distros` from `c.Config`.
3. If `--dry-run`: take a snapshot read (no lock needed), build `known` set,
   call `scanPoolForOrphans`/`scanPoolForOrphansAcrossDistros`, print
   `would remove <path>` per orphan, exit `exitOK`.
4. Else: `r.WithWriterLock(...)` to serialize against concurrent `add`
   (same rationale as `verify --gc` today — an in-flight add writes pool
   bytes before its row commits). Inside the lock:
   - Build `known`: `SELECT DISTINCT distro, filename FROM packages`
     filtered by scope; build the disk-relative set with `pool.DiskPath`.
   - Call `scanPoolForOrphans` / `scanPoolForOrphansAcrossDistros`.
   - Call `removeOrphans`.
5. Summary line: `gc: removed N orphan(s)` (or
   `would remove N orphan(s)` in dry-run). Per-orphan lines always print;
   `--quiet` suppresses only the trailing count line, matching
   `verify.printReport`.
6. Always exit `exitOK` on success: orphan presence is not a failure;
   `gc` is the action that resolves them.

**`delete` simplification** (`cmd/repokit/delete.go`):

- `runDelete` strips the pool loop, the `poolErr`/`poolErrIdx` accounting,
  the partial-failure branch in `report`, the long block comment on lock
  motivation.
- Two options for the DB call:
  - **Option A** (simpler): keep `WithWriterLock(…) DeleteConn(…)`. The
    lock is now a `DeleteConn` plumbing requirement (SQLite already
    serializes writers by itself; `DeleteConn`'s contract is "must run on
    a writer-locked connection") rather than an active design choice.
  - **Option B**: `tx := r.DB().BeginTx(…)`; `r.Delete(ctx, tx, filter)`;
    `tx.Commit()`. Drops the lock dependency but adds tx plumbing.
  - **Decision**: Option A. Less churn, identical semantics, no real cost.
- `report`'s tail line becomes:
  `deleted N package(s) — run 'repokit publish ...' to update indexes; orphan pool files removed via 'repokit gc'`
- Drop `pkg/pool` import if no longer referenced.

**`verify` simplification** (`cmd/repokit/verify.go`):

- Drop `GC bool` field.
- Drop `verifyAndGC` (its body is the `--gc`-only path).
- Drop `removeOrphans` and `matchOrphanDistro` (moved to `orphans.go` in
  Task 1).
- `run` collapses to just the `verifyOnly` branch; no `WithWriterLock`.
- `verifyReport.passed` drops the `gc` parameter — orphans always make
  verify fail (operator's signal that `gc` should run).
- `printReport` drops the `(removed)` suffix branch.

**Wiring** (`cmd/repokit/main.go`):

- New constant: `cmdGc = "gc"`.
- `Options.Gc GcCmd \`command:"gc" description:"remove orphan pool files (files with no database row)"\``.
- Dispatch: `case cmdGc: return opts.Gc.Execute(ctx)`.
- `needsPublic`: add `cmdGc` to the case list (L364).
- `needsConfig`: add `cmdGc` to whatever the equivalent line is.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs, plan
  cleanup — all in-repo.
- **Post-Completion** (no checkboxes): manual sanity check of an apt
  client against a freshly-deleted-but-not-yet-published suite (proves the
  win); changelog/announce note for the breaking removal of `verify --gc`.

## Implementation Steps

### Task 1: Extract orphan-walk helpers into `cmd/repokit/orphans.go`

**Files:**
- Create: `cmd/repokit/orphans.go`
- Create: `cmd/repokit/orphans_test.go`
- Modify: `cmd/repokit/verify.go`

- [ ] create `cmd/repokit/orphans.go`; move `scanOrphans` (rename to
      `scanPoolForOrphans`), `scanOrphansAcrossDistros` (rename to
      `scanPoolForOrphansAcrossDistros`), `removeOrphans`, and
      `matchOrphanDistro` verbatim from `verify.go`
- [ ] keep `scopePoolDir` and `poolDirName` co-located with the helpers
      (move them too — they are only consumed by the orphan walk)
- [ ] in `verify.go`, replace the moved-function call sites with calls into
      the new helpers; verify imports prune cleanly
- [ ] write `orphans_test.go` covering the walk: `.tmp`/`.bak` skip, the
      scoped (distro/suite) path, the unscoped (distros list) path, and
      the longest-prefix `matchOrphanDistro` rule
- [ ] run `make test` and `make lint` — must pass with no behavior change
      visible to existing tests

### Task 2: Add `gc` subcommand

**Files:**
- Create: `cmd/repokit/gc.go`
- Create: `cmd/repokit/gc_test.go`
- Modify: `cmd/repokit/main.go`

- [ ] add `cmdGc = "gc"` constant and `Gc GcCmd` field on `Options`; add
      dispatch case; add `cmdGc` to `needsPublic` and `needsConfig`
- [ ] create `cmd/repokit/gc.go` with `GcCmd`, `Execute`, `run`, flag
      validation, and the writer-locked scan/remove flow described in
      *Technical Details*
- [ ] build `known` via a focused query
      (`SELECT DISTINCT distro, filename FROM packages [WHERE distro=? [AND suite=?]]`);
      no hashing, no per-row file I/O
- [ ] write `gc_test.go` covering: happy path (orphan removed, parents
      pruned), scoped `--distro` only, scoped `--distro` + `--suite`,
      `--dry-run` leaves files on disk and exits OK, empty pool tree,
      cross-distro isolation (gc scoped to one distro leaves others alone),
      `.tmp`/`.bak` skip, `--suite` without `--distro` rejected as usage
      error
- [ ] write a race/concurrency test for `gc` modeled on
      `TestVerifyGCSerializesWithConcurrentWriter` in
      `verify_test.go` — port the structure (writer-lock holds across
      the walk so an in-flight `add` does not lose its pool bytes)
- [ ] run `make test` and `make lint` — new command works end-to-end

### Task 3: Remove `--gc` from `verify`

**Files:**
- Modify: `cmd/repokit/verify.go`
- Modify: `cmd/repokit/verify_test.go`

- [ ] drop `GC bool` field from `VerifyCmd`
- [ ] drop `verifyAndGC` and the `cmd.GC` branch in `run`; collapse to the
      `verifyOnly` path; remove the `WithWriterLock` usage (verify is now
      read-only)
- [ ] drop the `gc` parameter from `verifyReport.passed`; update callers
- [ ] drop the `(removed)` branch in `printReport`; orphans always print
      as `orphan <path>` with verify returning `exitError` when any exist
- [ ] update `verify_test.go`: drop all `--gc`-flag tests including
      `TestVerifyGCSerializesWithConcurrentWriter` (the gc race test
      lives in `gc_test.go` after Task 2); tighten the remaining
      orphan-reporting cases to assert verify now exits non-zero
      whenever orphans are present
- [ ] run `make test` and `make lint`

### Task 4: Make `delete` DB-only

**Files:**
- Modify: `cmd/repokit/delete.go`
- Modify: `cmd/repokit/delete_test.go`

- [ ] in `runDelete`, drop the `pool.Delete` + `pool.PruneEmptyParents`
      loop, drop `poolErr`/`poolErrIdx` accounting, drop the partial-
      failure branch in `report`
- [ ] keep `WithWriterLock` + `DeleteConn` (Option A from *Technical
      Details*); rewrite the block comment to a one-liner noting that
      `DeleteConn` requires a writer-locked connection (not an active
      serialization choice)
- [ ] update `report`'s closing line to point at both `publish` and `gc`
      (full text in *Technical Details*)
- [ ] drop `pkg/pool` import if unused
- [ ] update `delete_test.go` — concrete inventory:
      - `TestDeleteByNameAndVersion`: flip the post-delete pool-file
        assertions from `NoFileExists` to `FileExists`
      - `TestDeleteNamespacedDistroPrunes`: same flip; the prune
        behavior moves to `gc_test.go` so this test loses its
        "Prunes" angle — rename to `TestDeleteNamespacedDistro` and
        drop the prune assertions (gc_test.go covers them)
      - `TestDeleteScopedToSuite`: same flip on the pool-file
        assertions
      - `TestDeletePrunesEntireSuite`: the entire premise is now
        gc's responsibility — delete this test and re-create the
        equivalent in `gc_test.go` (covered by Task 2's happy-path
        + prune-empty-parents bullets, so just delete here)
      - `TestDeletePoolUnlinksTargetPublicDir`: entire premise gone
        (delete no longer unlinks); delete this test
      - `TestDeleteSerializesWithConcurrentWriter`: keep, but rewrite
        the docstring and the `poolFileCount == 0` assertion — the
        test now proves "delete blocks on the writer lock", not the
        old DB+pool race; assert DB row count instead of pool file
        count
- [ ] add one positive test (or extend `TestDeleteByNameAndVersion`)
      that asserts the pool file is **still present on disk** after
      delete (i.e. now an orphan) — this is the load-bearing positive
      assertion of the whole change
- [ ] run `make test` and `make lint`

### Task 5: Update integration tests

**Files:**
- Modify: `cmd/repokit/integration_test.go`

- [ ] flip the concrete assertion at `integration_test.go` L230-234
      (the `deletedPool` `ErrorIsf ... fs.ErrNotExist` block in
      `TestIntegration_MultiSuiteMultiArchMultiComponent`): the file
      must now still exist after delete. Add a follow-on `gc` call in
      the same test and re-assert `fs.ErrNotExist` after that
- [ ] add a fresh integration case that exercises the full new flow:
      `add → delete → assert pool file still on disk → publish → assert published Packages does not list deleted package → gc → assert pool file gone`
- [ ] grep for `verify --gc` invocations; replace any with `gc`
- [ ] `integration_realpkg_test.go` was audited and has no pool-
      deletion assertions; no changes expected there (re-verify
      during implementation in case of drift)
- [ ] run `make test` and `make race`

### Task 6: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/usage.md`

- [ ] update CLAUDE.md "Architecture" — the "Flow of a typical write
      command" bullets should note that `delete` only touches the DB now,
      and `gc` is the operator-driven reaper
- [ ] add `gc` to the subcommand list / `needsPublic` description
- [ ] update CLAUDE.md "Gotchas" — drop or rephrase any item that talks
      about delete's pool unlink + writer-lock motivation; mention that
      `verify` is read-only and `verify --gc` no longer exists; mention
      that orphans accumulate until `gc` runs (operator responsibility)
- [ ] (optional, piggybacked) `s/root.go/main.go/` sweep in CLAUDE.md
      where stale references survive from commit `d52f5fc`'s
      root.go→main.go merge; decide once and call out in the commit
      message
- [ ] rewrite the `--gc` section of `docs/usage.md` (7 references at
      L253, L256, L263, L266, L267, L270, L278) — the
      "verify --gc" subsection becomes a "gc" subsection; the "Pool /
      DB consistency" subsection (around L270) needs its ownership
      boundary rephrased to reflect that `delete` no longer touches
      the pool
- [ ] grep `README.md` once more as a sanity check (current count: 0
      `--gc` hits); only edit if the grep finds something
- [ ] no test step (docs only)

### Task 7: Verify acceptance criteria

- [ ] all five brainstorm decisions are reflected in code
- [ ] `repokit delete --distro X --suite Y --name pkg` leaves pool file on
      disk; subsequent `repokit publish` rebuilds indexes without it;
      subsequent `repokit gc` removes the file and prunes empty parents
- [ ] `repokit verify` reports orphans and exits non-zero; never removes
- [ ] `repokit verify --gc` errors out as an unknown flag
- [ ] `gc --suite X` without `--distro` returns a usage error
- [ ] `make test`, `make race`, `make lint` all green
- [ ] shell completion lists `gc` (smoke-test:
      `GO_FLAGS_COMPLETION=verbose .bin/repokit ''` includes `gc`)

### Task 8: Move plan to completed

- [ ] `mkdir -p docs/plans/completed`
- [ ] `git mv docs/plans/20260516-defer-pool-removal-and-split-gc.md docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes,
informational only*

**Manual verification:**
- against a running apt client: `add` a package, `publish`, install it,
  then `delete` + `publish` *without* running `gc`. Confirm `apt-get
  update` succeeds (no 404 on the removed `.deb`) — the win this plan
  was designed for.

**External notes:**
- this drops `verify --gc` without a deprecation alias. If anyone has
  cron jobs or scripts calling it, they need to switch to `repokit gc`.
  Mention in the next release notes.
