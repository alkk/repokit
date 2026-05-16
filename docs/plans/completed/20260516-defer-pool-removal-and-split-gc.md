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

- [x] create `cmd/repokit/orphans.go`; move `scanOrphans` (rename to
      `scanPoolForOrphans`), `scanOrphansAcrossDistros` (rename to
      `scanPoolForOrphansAcrossDistros`), `removeOrphans`, and
      `matchOrphanDistro` verbatim from `verify.go`
- [x] keep `scopePoolDir` and `poolDirName` co-located with the helpers
      (move them too — they are only consumed by the orphan walk)
- [x] in `verify.go`, replace the moved-function call sites with calls into
      the new helpers; verify imports prune cleanly
- [x] write `orphans_test.go` covering the walk: `.tmp`/`.bak` skip, the
      scoped (distro/suite) path, the unscoped (distros list) path, and
      the longest-prefix `matchOrphanDistro` rule
- [x] run `make test` and `make lint` — must pass with no behavior change
      visible to existing tests

### Task 2: Add `gc` subcommand

**Files:**
- Create: `cmd/repokit/gc.go`
- Create: `cmd/repokit/gc_test.go`
- Modify: `cmd/repokit/main.go`

- [x] add `cmdGc = "gc"` constant and `Gc GcCmd` field on `Options`; add
      dispatch case; add `cmdGc` to `needsPublic` and `needsConfig`
      (note: `needsConfig` returns true via its `default:` branch for every
      command except init/info/completion, so `gc` is already covered with
      no edit — only `needsPublic` needed the explicit `cmdGc` case)
- [x] create `cmd/repokit/gc.go` with `GcCmd`, `Execute`, `run`, flag
      validation, and the writer-locked scan/remove flow described in
      *Technical Details*
- [x] build `known` via a focused query
      (`SELECT DISTINCT distro, filename FROM packages [WHERE distro=? [AND suite=?]]`);
      no hashing, no per-row file I/O (new `repo.PoolRefs` /
      `repo.PoolRefsConn` + `knownPoolSet`)
- [x] write `gc_test.go` covering: happy path (orphan removed, parents
      pruned), scoped `--distro` only, scoped `--distro` + `--suite`,
      `--dry-run` leaves files on disk and exits OK, empty pool tree,
      cross-distro isolation (gc scoped to one distro leaves others alone),
      `.tmp`/`.bak` skip, `--suite` without `--distro` rejected as usage
      error (plus namespaced-distro prune, unconfigured-distro abort, and
      unsafe-scope rejection; `repo` query helpers unit-tested in
      `queries_test.go`)
- [x] write a race/concurrency test for `gc` modeled on
      `TestVerifyGCSerializesWithConcurrentWriter` in
      `verify_test.go` — port the structure (writer-lock holds across
      the walk so an in-flight `add` does not lose its pool bytes)
      (`TestGcSerializesWithConcurrentWriter`)
- [x] run `make test` and `make lint` — new command works end-to-end

### Task 3: Remove `--gc` from `verify`

**Files:**
- Modify: `cmd/repokit/verify.go`
- Modify: `cmd/repokit/verify_test.go`

- [x] drop `GC bool` field from `VerifyCmd`
- [x] drop `verifyAndGC` and the `cmd.GC` branch in `run`; collapse to the
      `verifyOnly` path; remove the `WithWriterLock` usage (verify is now
      read-only). Also inlined the now-single-caller `scanOrphansForScope`
      into `verifyOnly` per *Technical Details*, and dropped the now-unused
      `database/sql` import.
- [x] drop the `gc` parameter from `verifyReport.passed`; update callers
- [x] drop the `(removed)` branch in `printReport`; orphans always print
      as `orphan <path>` with verify returning `exitError` when any exist
- [x] update `verify_test.go`: dropped the pure gc-mechanics tests
      (`TestVerifyGCRemovesOnlyOrphan`, `…KeepsSiblingTreeIntact`,
      `…PrunesNamespacedDistro`, `…DoesNotRemoveRowsForMissingPoolFiles`,
      `TestVerifyGCSerializesWithConcurrentWriter` — all covered by
      `gc_test.go`/`orphans_test.go`); converted the scoping/safety/
      reporting cases to the read-only contract (no `--gc`, assert
      `exitError` whenever orphans are present, files always survive);
      added `TestVerifyGCFlagRejected` proving `verify --gc` is now an
      unknown-flag usage error
- [x] run `make test` and `make lint`

### Task 4: Make `delete` DB-only

**Files:**
- Modify: `cmd/repokit/delete.go`
- Modify: `cmd/repokit/delete_test.go`

- [x] in `runDelete`, drop the `pool.Delete` + `pool.PruneEmptyParents`
      loop, drop `poolErr`/`poolErrIdx` accounting, drop the partial-
      failure branch in `report`
- [x] keep `WithWriterLock` + `DeleteConn` (Option A from *Technical
      Details*); rewrite the block comment to a one-liner noting that
      `DeleteConn` requires a writer-locked connection (not an active
      serialization choice)
- [x] update `report`'s closing line to point at both `publish` and `gc`
      (full text in *Technical Details*)
- [x] drop `pkg/pool` import if unused
- [x] update `delete_test.go` — concrete inventory:
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
      - ➕ `TestDeleteAllVersions`: not in the original inventory but
        carried prune assertions that no longer hold under DB-only
        delete; converted to assert the pool tree stays intact
        (orphan left behind, no pruning)
- [x] add one positive test (or extend `TestDeleteByNameAndVersion`)
      that asserts the pool file is **still present on disk** after
      delete (i.e. now an orphan) — this is the load-bearing positive
      assertion of the whole change (extended `TestDeleteByNameAndVersion`)
- [x] run `make test` and `make lint`
- ➕ `integration_test.go` L234 (`TestIntegration_MultiSuiteMultiArch‐
      MultiComponent`): the pre-existing `fs.ErrNotExist` assertion broke
      under DB-only delete, so flipped it to `assert.FileExists` here to
      keep `make test` green (mandated "update existing tests when
      behavior changes"). Task 5's remaining items — the follow-on `gc`
      call, the new full-flow integration case, the `verify --gc` grep —
      are untouched and still open under Task 5.

### Task 5: Update integration tests

**Files:**
- Modify: `cmd/repokit/integration_test.go`

- [x] flip the concrete assertion at `integration_test.go` L230-234
      (the `deletedPool` block in
      `TestIntegration_MultiSuiteMultiArchMultiComponent`): the
      `FileExists` flip already landed under Task 4; this task added the
      follow-on `runCmd("gc", "--distro", "debian")` call and the
      `require.ErrorIsf(... fs.ErrNotExist)` re-assertion (plus a
      `FileExists` check that the still-referenced bullseye survivor is
      untouched by gc)
- [x] add a fresh integration case that exercises the full new flow:
      `TestIntegration_DeleteDefersPoolRemovalUntilGc` —
      `add alpha+beta → publish → delete alpha → assert alpha pool file
      still on disk → publish → assert rebuilt Packages drops alpha but
      keeps beta and alpha's pool file STILL survives the republish →
      gc → assert alpha pool gone, beta pool (DB-referenced) untouched`.
      Uses a beta survivor so the post-delete Packages file still exists
      (an emptied component/arch is skipped by `writeIndexes`)
- [x] grep for `verify --gc` invocations; replace any with `gc` — the
      grep surfaced stale references beyond `integration_test.go`:
      fixed the three user-facing `run 'repokit verify --gc' to reclaim`
      stderr strings + two comments in `cmd/repokit/add.go`, the
      `add/delete/verify --gc/concurrent` comment in
      `cmd/repokit/publish.go`, and the example-caller comments in
      `pkg/pool/pool.go` and `pkg/repo/repo.go`. The intentional
      `verify --gc`-rejection docstring/test in `verify_test.go` is left
      as-is (it documents the breaking removal)
- [x] `integration_realpkg_test.go` was audited and has no pool-
      deletion assertions; re-verified — its only `fs.ErrNotExist`
      assertion (L145) is the "no binary-all directory" check, unrelated
      to delete; no changes made
- [x] run `make test` and `make race` — both green (make test 83.5%
      coverage; make race all packages ok; make lint also run, 0 issues)

### Task 6: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/usage.md`

- [x] update CLAUDE.md "Architecture" — the "Flow of a typical write
      command" bullets should note that `delete` only touches the DB now,
      and `gc` is the operator-driven reaper (step 3 now states `delete`
      is DB-only; added a dedicated "Orphan reaping is split out" paragraph
      covering the delete→publish consistency window, the explicit `gc`
      reaper, the shared `orphans.go` helpers, and the read-only `verify`)
- [x] add `gc` to the subcommand list / `needsPublic` description
      (`Context.PublicDir` bullet now lists `gc` in the `needsPublic`
      set; `Context.Config` bullet adds `gc` to the
      `requireConfiguredDistro` enumeration)
- [x] update CLAUDE.md "Gotchas" — no existing item described delete's
      pool-unlink + writer-lock motivation (it lived in code comments,
      swept in Task 5), so added two new gotchas: (1) `delete` is DB-only
      and orphans accumulate until `gc` (operator responsibility, never
      auto-pruned, "don't re-add the unlink"); (2) `verify` is read-only
      with no `--gc` flag, plus the shared writer-lock-spanning-the-scan
      invariant
- [x] (optional, piggybacked) `s/root.go/main.go/` sweep done — all six
      stale `root.go` references in CLAUDE.md (Architecture intro,
      `Context.PublicDir`, `Context.Config`, scope-validation,
      `resolvePublicDir`, `defaultStateDir` gotcha) now point at
      `main.go`; commit `d52f5fc` confirmed as the root.go→main.go merge.
      Called out in the commit message
- [x] rewrite the `--gc` section of `docs/usage.md` — `verify` subsection
      rewritten as strictly read-only (no `--gc` flag, exits non-zero on
      any finding, never acts); a new `gc` subsection added after it
      (flags, output, lock semantics, scope rules, dry-run) with a
      "Migrating from `verify --gc`" callout; the `delete` subsection's
      ownership boundary rephrased to "DB-only, leaves orphans" and its
      stale "after each pool file is removed … pruned" paragraph replaced;
      `add/delete/publish/verify` enumerations (public-dir resolution,
      scope-validation, init `config.yaml` note, config table `distros`
      and `public_dir` required-for rows) all extended with `gc`
- [x] grep `README.md` once more as a sanity check — 0 `--gc` /
      `verify --gc` / `gc` hits confirmed; no edit needed
- [x] no test step (docs only)

### Task 7: Verify acceptance criteria

- [x] all five brainstorm decisions are reflected in code (verified by
      inspection: (1) `delete.go` is DB-only with no `pkg/pool` import and
      `publish.go` has no prune logic — pool removal lives only in
      `gc.go`'s `removeOrphans`; (2) `verify.go` has no `GC` field,
      `TestVerifyGCFlagRejected` proves the flag is gone; (3) `main.go`
      defines `cmdGc = "gc"` + dispatch + `needsPublic`; (4) `GcCmd` has
      only `Distro`/`Suite`/`DryRun` — orphan-only, no `--name`/`--sha256`;
      (5) `gc.go` builds `known` via `PoolRefs`/`knownPoolSet` with no
      hashing)
- [x] `repokit delete --distro X --suite Y --name pkg` leaves pool file on
      disk; subsequent `repokit publish` rebuilds indexes without it;
      subsequent `repokit gc` removes the file and prunes empty parents
      (proven end-to-end by `TestIntegration_DeleteDefersPoolRemovalUntilGc`
      — PASS)
- [x] `repokit verify` reports orphans and exits non-zero; never removes
      (`TestVerifyOrphanDetected` — PASS: `exitError`, `orphan …` printed,
      `FileExists` after run, no `(removed)`)
- [x] `repokit verify --gc` errors out as an unknown flag
      (`TestVerifyGCFlagRejected` — PASS: `exitUsage`, stderr "unknown
      flag" + "gc")
- [x] `gc --suite X` without `--distro` returns a usage error
      (`TestGcSuiteWithoutDistroIsUsageError` — PASS: `exitUsage`, stderr
      "--suite requires --distro")
- [x] `make test`, `make race`, `make lint` all green (make test: all
      packages ok, 83.5% coverage; make race: all packages ok;
      golangci-lint: 0 issues — run with a writable `GOCACHE` /
      `GOLANGCI_LINT_CACHE` since the sandbox `~/.cache` is read-only)
- [x] shell completion lists `gc` (smoke-test:
      `GO_FLAGS_COMPLETION=verbose .bin/repokit ''` output includes `gc`,
      alongside add/completion/delete/info/init/list/publish/search/verify)

### Task 8: Move plan to completed

- [x] `mkdir -p docs/plans/completed`
- [x] `git mv docs/plans/20260516-defer-pool-removal-and-split-gc.md docs/plans/completed/`

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
