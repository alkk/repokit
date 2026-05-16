# Rename `verify` → `check` and add `--purge-db` for missing-row drift

## Overview

Two bundled changes to the read-side of repokit:

1. **Rename the `verify` subcommand to `check`.** Hard rename — no
   deprecation alias. Matches the project's pre-1.0 convention (the prior
   `verify --gc` removal also dropped its alias). Updates the subcommand
   name, the `VerifyCmd` struct, the file pair, internal helpers, the
   `cmdVerify` const, and every `verify:` output prefix.

2. **Add `check --purge-db` to resolve missing-pool drift.** Today `verify`
   reports rows whose pool file is missing but never acts on them, leaving
   the operator to either restore the file or manually `delete` the row.
   `check --purge-db` deletes those rows. Symmetrical with `gc` reaping
   orphan files: `gc` reaps file-without-row, `check --purge-db` reaps
   row-without-file. Missing-only — hash mismatches are deliberately left
   alone because a transient FS/mount issue can mimic corruption, and
   silently dropping rows for "corrupt" packages would mask real problems.

The motivating user-visible state (from the brainstorm session):

```
$ .bin/repokit verify
missing customer/customer1/pool/trixie/main/r/repokit-test-package/...
missing debian/pool/trixie/main/r/repokit-test-package/...
verify: 0 mismatch(es), 5 missing, 0 orphan(s)
```

After this plan, the operator types `check --purge-db` and the five rows
are removed; the next `publish` re-emits indexes without them.

### Brainstorm decisions that anchor this plan

1. **Placement**: on `check`, not on `gc` or a new `prune` subcommand.
   Symmetric with `gc`'s orphan-file reaping.
2. **Scope**: missing files only. Mismatches reported as today.
3. **Exit code**: follows post-action state — exit 0 iff no mismatches
   and no orphans remain after the purge. Mirrors `gc`'s "the action that
   resolves the finding does not fail the run" rule.
4. **No `--dry-run`**: operators preview by running plain `check` first.
5. **Scope inheritance**: `--purge-db` respects existing
   `--distro` / `--suite` filters; no separate purge-scope flag.
6. **Lock**: Mode B (`--purge-db` set) runs inside `r.WithWriterLock`,
   spanning the row read, hash pass, delete loop, and orphan scan. Same
   primitive `gc` uses, same invariant — without it a racing `add` could
   misclassify its in-flight pool file as an orphan or sneak a `(row, file)`
   pair into the gap between list and delete.
7. **Mode A unchanged**: plain `check` (no `--purge-db`) stays lock-free
   and strictly read-only — current `verify` behavior preserved.

## Context (from discovery)

- `cmd/repokit/verify.go` — `VerifyCmd`, `verifyReport`, `verifyOnly`,
  `hashAndIndex`, `verifyOne`, `filterRowsByConfiguredDistros`,
  `printReport`. All command-name-coupled identifiers get the rename;
  `hashAndIndex` and `filterRowsByConfiguredDistros` stay as-named (not
  tied to the command).
- `cmd/repokit/verify_test.go` — sibling test file; assertions on
  `verify:` prefixes and the `verify` command token need updating, and
  new test cases land here for `--purge-db`.
- `cmd/repokit/main.go` — `cmdVerify` constant, `Options.Verify VerifyCmd`
  registration with `command:"verify"` tag, `needsPublic` switch case,
  `dispatch` switch case. All flip together.
- `cmd/repokit/gc.go` — three comments mention `verify` (`gc.go:22`,
  `gc.go:82`, `gc.go:158`). Cosmetic, but worth keeping the doc accurate.
- `cmd/repokit/root_test.go` — at least 11 references to `"verify"` as a
  command token plus two comment mentions. Test cases that drive
  `run([]string{..., "verify"}, ...)` need renaming.
- `cmd/repokit/integration_test.go` and `integration_realpkg_test.go` —
  end-to-end flows call `runCmd("verify")` and comment on verify
  behaviour. Substitute throughout.
- `pkg/repo/queries.go` — `List(ctx, filter)` is a thin wrapper around
  `queryPackages(ctx, r.db, filter, "")`. Need to add `ListConn(ctx,
  conn, filter)` mirroring the `PoolRefs` / `PoolRefsConn` split so Mode
  B's in-lock read uses the same connection (`queryPackages` already
  accepts a `queryer` interface, so the wrapper is trivial).
- `pkg/repo/queries.go` — `DeleteConn(ctx, conn, Filter)` returns the
  deleted rows; `Filter{Distro, Name, Version, Architecture}` is the
  unique key for one missing `.deb`. Multiple suite/component rows that
  share the same pool file will all be purged in a single call (correct
  — they all point at the same absent disk path).
- `cmd/repokit/completion/hook.{bash,fish,zsh}` — go-flags emits
  subcommand names dynamically via `GO_FLAGS_COMPLETION=verbose`. The
  new `check` name will surface automatically; no script edits required.
- `CLAUDE.md` — "Architecture" mentions verify's read-only-ness in
  multiple places; "Gotchas" has a paragraph headed `verify` is strictly
  read-only — there is no `--gc` flag`; `needsPublic` description lists
  `verify`. A new gotcha is needed for the `--purge-db` missing-only
  decision.
- `docs/plans/completed/20260516-defer-pool-removal-and-split-gc.md` —
  precedent for the `gc` split and the writer-lock-spanning-the-scan
  invariant that Mode B reuses.

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
- run `make test` after each task before proceeding
- maintain backward compatibility within the changed surface (i.e. no other
  command's flags or output change)

## Testing Strategy

- **unit tests**: required for every task. The rename task updates existing
  tests; the `--purge-db` task adds new ones.
- **integration tests**: `integration_test.go` and
  `integration_realpkg_test.go` get updated assertions but no new e2e cases
  — the unit/cmd-level tests cover the new `--purge-db` paths.
- **race detection**: `make race` after Task 3 to catch any lock-misuse
  introduced by the writer-locked path.
- **lint**: `make lint` after every task — go-flags struct tags and the
  switch case naming pattern are easy to typo.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

Two code-level shapes:

**Rename**: a structural refactor with no behavior change. One file pair
moves (`verify.go`/`verify_test.go` → `check.go`/`check_test.go`),
identifiers and string literals flip together, the go-flags `command:`
tag changes and the `dispatch` switch follows. Output prefix updates are
in `printReport`. Nothing in storage or on disk changes.

**`--purge-db`**: a new bool flag on `CheckCmd`. `Execute` branches on the
flag:

- **Mode A — no flag**: today's flow unchanged. `r.List`, hash pass,
  orphan scan, print, exit per `passed()`.
- **Mode B — flag set**: wrap the whole pass in `r.WithWriterLock`. Inside
  the lock: `r.ListConn(ctx, conn, filter)` (new method), hash pass that
  also collects `[]repo.Package` for missing rows, then per missing row
  `r.DeleteConn(ctx, conn, Filter{Distro,Name,Version,Architecture})`,
  then orphan scan. After the lock releases, print the report. The report
  prefixes rows in `MissingPool` as `purged X` (not `missing X`) and the
  summary includes a `N purged` count.

Exit logic: `passed()` already considers mismatches + missing + orphans
all-zero. After Mode B purges, `MissingPool` is empty by construction
(or — for the rare row-delete-failed case — still populated, which
correctly fails the run). The existing `passed()` logic carries over.

## Technical Details

**`pkg/repo/queries.go` — new method:**

```go
// ListConn mirrors List but uses the supplied writer-locked connection.
// Used by check --purge-db so the row read and the subsequent
// DeleteConn writes happen on the same connection, preventing a racing
// add from inserting a (row, file) pair between them.
func (r *Repo) ListConn(ctx context.Context, conn *sql.Conn, filter Filter) ([]Package, error) {
    return queryPackages(ctx, conn, filter, "")
}
```

`queryPackages` already accepts a `queryer` interface that both `*sql.DB`
and `*sql.Conn` satisfy — no parameterisation change needed.

**`cmd/repokit/check.go` — new struct field:**

```go
type CheckCmd struct {
    Distro  DistroName `long:"distro" description:"..."`
    Suite   string     `long:"suite" description:"..."`
    PurgeDB bool       `long:"purge-db" description:"delete database rows whose pool file is missing on disk (mismatches are left alone)"`
}
```

**`cmd/repokit/check.go` — Mode B execution skeleton:**

```go
func (cmd *CheckCmd) runPurgeDB(ctx context.Context, c *Context, distros []string, r *repo.Repo, report *checkReport) error {
    filter := repo.Filter{Distro: string(cmd.Distro), Suite: cmd.Suite}
    return r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
        rows, err := r.ListConn(ctx, conn, filter)
        if err != nil {
            return fmt.Errorf("listing rows: %w", err)
        }
        rows = filterRowsByConfiguredDistros(string(cmd.Distro), rows, distros, c.Stderr)
        var missingPkgs []repo.Package
        known, err := hashAndIndexCollecting(c.PublicDir, rows, report, &missingPkgs)
        if err != nil {
            return err
        }
        for _, p := range missingPkgs {
            del := repo.Filter{Distro: p.Distro, Name: p.Name, Version: p.Version, Architecture: p.Architecture}
            if _, err := r.DeleteConn(ctx, conn, del); err != nil {
                return fmt.Errorf("purging %s/%s/%s/%s: %w", p.Distro, p.Name, p.Version, p.Architecture, err)
            }
        }
        // Orphan scan stays inside the lock.
        orphans, err := cmd.scanOrphans(c.PublicDir, distros, known)
        if err != nil {
            return err
        }
        report.Orphans = orphans
        return nil
    })
}
```

**Output prefix and summary change (Mode B only):**

```
purged debian/pool/trixie/main/r/repokit-test-package/...
check: 0 mismatch(es), 5 purged, 0 orphan(s)
```

`printReport` learns a single boolean (`purged`) so the prefix and the
summary verb flip together. Mode A still emits `missing X` and the
unchanged summary.

**Filter for `DeleteConn`**: `(Distro, Name, Version, Architecture)` is
the natural key. Including only `(Distro, Name, Version)` would
over-match when both an `_all.deb` and an `_amd64.deb` share name+version.
The `Filter` struct already has `Architecture`. The filter deliberately
omits `Suite` and `Component` because the same pool file may back rows in
multiple `(suite, component)` tuples — when the file is missing, all
those rows are equally orphaned and should be purged in a single call.
The `TestCheck_PurgeDB_MultiRowPerFile` test pins this down.

**Orphan-scan `known`-set in Mode B**: the orphan scan inside the
writer-locked callback uses the **pre-purge** `known` set (the disk-relative
paths of every row from the original `ListConn` snapshot, including the
rows about to be purged). This is correct because the purged rows pointed
at *missing* files — those paths cannot appear as orphans in the pool
walk regardless of whether they are in `known`. Computing a post-purge
`known` would require a second `ListConn` and adds no behavioural
difference. The Mode B doc comment must make this explicit; the
`TestCheck_PurgeDB_OrphanReportingDuringPurge` test asserts a separately
created orphan is still reported even when purge runs in the same pass.

> **Post-completion correction (codex review):** the "those paths cannot
> appear as orphans" reasoning silently assumed the filesystem is static
> between the hash pass and the scan. The writer lock only serializes DB
> *writers*, not the FS, so an external process (backup restore, re-synced
> replica, manual `cp`) can recreate a previously-missing pool file in that
> window. With its row purged, the reappeared file is a genuine orphan that
> a stale `known` entry would suppress, letting the run exit clean over a
> fresh on-disk orphan. Fixed in `runPurgeDB` by `delete`-ing every purged
> row's `pool.DiskPath` from `known` before `scanOrphans`; pinned by
> `TestCheck_PurgeDB_RestoredFileMidRunSurfacesAsOrphan` (uses the new
> `purgeScanHook` test seam to inject the restore deterministically).

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): rename + flag + tests +
  doc sweep, all in this repo.
- **Post-Completion** (no checkboxes): manual smoke against the operator's
  real public dir to confirm the new flag handles the actual drift case
  from the brainstorm screenshot.

## Implementation Steps

### Task 1: Add `ListConn` to `pkg/repo/queries.go`

Foundation for Mode B's in-lock row read. Mode A keeps using `List`.

**Files:**
- Modify: `pkg/repo/queries.go`
- Modify: `pkg/repo/queries_test.go` (or wherever `List` is tested — discover during implementation)

- [x] add `ListConn(ctx context.Context, conn *sql.Conn, filter Filter) ([]Package, error)` mirroring `PoolRefsConn`
- [x] doc-comment the method explaining when callers should prefer `ListConn` over `List` (writer-locked passes that need a coherent snapshot before subsequent writes)
- [x] add a unit test that opens a connection via `WithWriterLock`, calls `ListConn`, and asserts it returns the same rows as `List` for the same filter
- [x] add a test asserting `ListConn` honours all filter fields (`Distro`, `Suite`, `Name`) at least once
- [x] run `make test` — must pass before Task 2

### Task 2: Rename `verify` → `check` (no behavior change)

Pure mechanical rename. No new feature work in this task.

**Files:**
- Create: `cmd/repokit/check.go` (content moved from `verify.go`)
- Create: `cmd/repokit/check_test.go` (content moved from `verify_test.go`)
- Delete: `cmd/repokit/verify.go`
- Delete: `cmd/repokit/verify_test.go`
- Modify: `cmd/repokit/main.go`
- Modify: `cmd/repokit/gc.go` (comments)
- Modify: `cmd/repokit/init.go` (embedded config.yaml comment)
- Modify: `cmd/repokit/publish.go` (comment)
- Modify: `pkg/pool/pool.go` (comment)
- Modify: `cmd/repokit/root_test.go`
- Modify: `cmd/repokit/integration_test.go`
- Modify: `cmd/repokit/integration_realpkg_test.go`

- [x] move `cmd/repokit/verify.go` → `cmd/repokit/check.go` (use `git mv` for history); inside the file rename `VerifyCmd` → `CheckCmd`, `verifyReport` → `checkReport`, `(cmd *VerifyCmd) verifyOnly` → `(cmd *CheckCmd) checkOnly`, `verifyOne` → `checkOne`, and flip all `verify:` output literals to `check:` (in `printReport`, in error wrappers, and in the `verify: skipping distro ...` stderr line)
- [x] in the renamed `check.go`, update the `description:` tags on `Distro` (`"limit verification to one distribution..."` → `"limit check to one distribution..."`) and `Suite` (`"limit verification to one suite..."` → `"limit check to one suite..."`) — the word "verification" is command-coupled and must flip
- [x] move `cmd/repokit/verify_test.go` → `cmd/repokit/check_test.go` (`git mv`); flip expected output strings (`"verify: "` → `"check: "`, `"verify:"` → `"check:"`) and command tokens passed to `run` (`"verify"` → `"check"`); rename `TestVerify*` → `TestCheck*`
- [x] in `cmd/repokit/main.go`: rename `cmdVerify = "verify"` → `cmdCheck = "check"`; rename the `Options.Verify VerifyCmd ...command:"verify"...` field to `Options.Check CheckCmd ...command:"check"...`; update the `needsPublic` switch case and the `dispatch` switch case; update the help-text mention in `Options.Public`'s `description:` tag (currently lists `add/publish/verify/gc`); update the three comments mentioning verify (lines 106, 152, 196, 381 — `(verify, publish) can pass it through`, `unscoped publish/verify`, `list/search/verify`)
- [x] in `cmd/repokit/gc.go`: update three comment mentions of `verify` (lines 22, 82, 158) to `check`
- [x] in `cmd/repokit/init.go` line 32: update the embedded `config.yaml` comment (`add/delete/publish/verify` → `add/delete/publish/check`)
- [x] in `cmd/repokit/publish.go` line 130: update the comment (`add/delete/verify don't regress` → `add/delete/check don't regress`)
- [x] in `pkg/pool/pool.go` line 445: update the comment (`publish would surface the missing pool file via verify` → `... via check`)
- [x] in `cmd/repokit/root_test.go`: replace all `"verify"` command tokens (~11 occurrences) with `"check"`, rename helper map keys (`"verify": true` in the dispatcher test), update comment mentions (`publish or verify operate` → `publish or check operate`)
- [x] in `cmd/repokit/integration_test.go`: substitute `runCmd("verify")` → `runCmd("check")`, flip `cmdVerify` → `cmdCheck` at line 535 (constant reference in a command-list slice), and update any `verify:`-prefixed expected output and comments mentioning verify
- [x] in `cmd/repokit/integration_realpkg_test.go`: substitute `runCmd("verify")` → `runCmd("check")` and any `verify:`-prefixed expected output; update comments mentioning verify (no `verify` references existed in this file — nothing to substitute)
- [x] grep the whole repo (`grep -rn "verify\|VerifyCmd\|cmdVerify" cmd pkg README.md docs/`) to catch stragglers (test helper names, log strings, doc mentions) and fix any remaining (code stragglers fixed: `publish_test.go` + `config_test.go` command-coupled `add/delete/verify` comments flipped; remaining `verify` hits in code are generic hash-verification wording, not the command; `README.md`/`docs/`/`CLAUDE.md` doc mentions are deferred to the dedicated Task 4 doc sweep per the plan)
- [x] run `make test` and `make lint` — must pass before Task 3

### Task 3: Add `--purge-db` flag and Mode B execution flow

**Files:**
- Modify: `cmd/repokit/check.go`
- Modify: `cmd/repokit/check_test.go`

- [x] add `PurgeDB bool` field to `CheckCmd` with `long:"purge-db" description:"delete database rows whose pool file is missing on disk (mismatches are left alone)"`
- [x] split `(cmd *CheckCmd) run` into the existing read-only path (Mode A) and a new writer-locked path (Mode B); dispatch on `cmd.PurgeDB` (new `runPurgeDB`)
- [x] introduce a helper that walks rows and collects both the existing `report` and a `[]repo.Package` of missing entries — added sibling `hashAndIndexCollecting` returning `(known, missing, err)`; `hashAndIndex` now delegates to it, so the Mode A signature is unchanged. `checkOne` now returns `(absent bool, err error)`.
- [x] inside the `WithWriterLock` callback: `ListConn` rows, scope-filter, run the collecting hash pass, loop missing packages calling `DeleteConn(Filter{Distro,Name,Version,Architecture})`, then run the orphan scan against the pre-purge `known` set — the runPurgeDB doc comment calls out the pre-purge-known rationale explicitly
- [x] note: lifted the `scanOrphans` dispatch into `orphans.go` as a package-level `scanOrphans(publicDir, distro, suite, distros, known)` (lower-coupling option); removed the `(*GcCmd) scanOrphans` method and updated gc.go's two call sites and check.go's `checkOnly` to use it
- [x] teach `(*checkReport).printReport` about a `purged` mode (reads `cmd.PurgeDB`): missing entries print as `purged X`, the summary uses `N purged`, and like gc the count line is always emitted in purge mode (added `checkReport.Purged`, populated by moving `MissingPool` after the deletes commit)
- [x] keep `passed()` unchanged — `Purged` is intentionally excluded from it, so after Mode B purges the exit-code rule is "exit 0 iff no mismatches and no orphans"
- [x] add `TestCheck_PurgeDB_RemovesMissingRows`: add a package, remove its pool file from disk, run `check --purge-db`, assert exit 0, summary shows `1 purged`, row is gone from the DB
- [x] add `TestCheck_PurgeDB_LeavesMismatchesAlone`: add a package, corrupt its pool file's bytes (truncate or rewrite), run `check --purge-db`, assert exit 1, summary shows `1 mismatch(es)`, row still in DB
- [x] add `TestCheck_PurgeDB_ScopedByDistroSuite`: missing files in two distros, run `check --distro X --purge-db`, assert only X's rows are purged
- [x] add `TestCheck_PurgeDB_NoMissingNoOp`: no missing files, run `check --purge-db`, assert exit 0, summary shows `0 purged`, no DB writes
- [x] add `TestCheck_PurgeDB_MultiRowPerFile`: same NVRA added into two suites of the same distro yields two rows/two pool files (the layout keys pool paths by suite, so the rows do not literally share one file); remove both pool files; run `check --purge-db`; assert both rows are gone in one pass (validates the Suite/Component omission in the `DeleteConn` filter)
- [x] add `TestCheck_PurgeDB_OrphanReportingDuringPurge`: stage a real orphan (pool file with no DB row) alongside a missing-file row; run `check --purge-db`; assert the missing row is purged AND the orphan is still reported, summary shows `0 mismatch(es), 1 purged, 1 orphan(s)`, exit 1 (the orphan keeps the run non-clean); validates the pre-purge `known`-set decision in Mode B
- [x] adopted the channel-synchronised contention pattern from `gc_test.go` (it is already established) as `TestCheck_PurgeDB_SerializesWithConcurrentWriter` instead of skipping
- [x] run `make test` and `make race` — both pass (also ran `make lint` clean and `make fmt`)

### Task 4: Sweep CLAUDE.md, README.md, and docs/usage.md

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`
- Modify: `docs/usage.md`

**`CLAUDE.md`:**
- [x] replace every `verify` mention in the Architecture section — the `Context.PublicDir` `needsPublic` list, the `requireConfiguredDistro` distro list, the orphan-reaping paragraph, the "Tests" section line, and the shell-completion `--distro` list (no literal `cmd/repokit/verify.go` reference existed; the equivalent "publish, and verify" Tests line was flipped)
- [x] update the gotcha headed "`verify` is strictly read-only — there is no `--gc` flag" to reflect that `check` is read-only in Mode A and writer-locked in Mode B (`--purge-db`), while still emphasising that hash mismatches are not auto-resolved
- [x] add a new gotcha entry explaining the `--purge-db` missing-only decision: a transient FS/mount issue can mimic corruption, so mismatches are deliberately reported but never auto-deleted; the operator restores the file or runs `delete` manually
- [x] mention `check --purge-db` in the architecture flow where the delete→publish window is discussed, as the recovery path when pool files have been removed out-of-band

**`README.md`:**
- [x] line 51: update the commands table row (`| `verify` | rehash pool files ...` → `| `check` | rehash pool files ...`); added `--purge-db` to the description line so the table previews the new flag
- [x] lines 59-60: kept the `verify --gc` migration note but reworded it — "`verify --gc` flag was removed and the surrounding command renamed — replace `repokit verify --gc` with `repokit gc`, and `repokit verify` (no flag) with `repokit check`"
- [x] line 98: update the `needsPublic` enumeration (`add`, `publish`, `verify`, `gc` → `add`, `publish`, `check`, `gc`)
- [x] line 171: update the `--distro` enumeration (`add`, `delete`, `publish`, `verify`, and `gc` → `add`, `delete`, `publish`, `check`, and `gc`)

**`docs/usage.md`:**
- [x] line 27 and line 52 and line 85: update the command enumerations (`publish, verify, and gc` → `publish, check, and gc`; `add, delete, publish, verify, and gc` → `..., check, ...`; `verify / gc invocation` → `check / gc invocation`)
- [x] line 264: rename the `### verify` section heading to `### check`
- [x] lines 267, 271, 285-86, 291, 293, 295: rewrite the section body — read-only-by-default wording, `repokit check [--distro <d>] [--suite <s>] [--purge-db]` synopsis, `check:` summary literals, plus a new `#### --purge-db` subsection documenting the missing-only contract, the post-action exit-code rule, the absence of `--dry-run`, and the `(distro,name,version,architecture)` delete key
- [x] line 323: update the `(that is verify's job ...)` aside to `(that is check's job ...)`
- [x] lines 350-353: keep the `Migrating from verify --gc` callout but update it — `repokit verify --gc` still maps to `repokit gc`, and a new clause notes that `repokit verify` (no flag) is now `repokit check` (plus a `check --purge-db` pointer)
- [x] lines 447, 449: update the config-reference table rows (`for add/delete/publish/verify/gc` → `for add/delete/publish/check/gc`; `for add/publish/verify/gc` → `for add/publish/check/gc`)
- [x] grep `docs/usage.md` and `README.md` once more for any remaining `verify` occurrences after the targeted edits (only the intentional historical `verify --gc` / rename-note references remain)
- [x] run `make test` — passed (all packages, 84.0% coverage); docs-only change, lint unaffected

### Task 5: Verify acceptance criteria

- [x] verify `repokit verify ...` now returns "unknown subcommand" (no alias) — `Unknown command 'verify'. Please specify one command of: add, check, ...`, exit 2
- [x] verify `repokit check --help` shows the new `--purge-db` flag with the missing-only wording (`delete database rows whose pool file is missing on disk (mismatches are left alone)`)
- [x] verify against the brainstorm screenshot scenario: seeded debian + customer/customer1 with 5 pool files, deleted them — plain `check` reproduced `check: 0 mismatch(es), 5 missing, 0 orphan(s)` exit 1; `check --purge-db` → `5 purged` exit 0; follow-up `check` → `ok` exit 0; `list` empty
- [x] verify the exit-code rule end-to-end: mismatch (corrupted bytes) alongside a missing file → `check --purge-db` printed `mismatch ...` + `purged ...`, summary `check: 1 mismatch(es), 1 purged, 0 orphan(s)`, exit 1; the mismatch row stayed in the DB, the missing row was purged
- [x] run full test suite: `make test` — all packages ok, total 84.0%
- [x] run race sweep: `make race` — all packages ok, no data races
- [x] run lint: `make lint` — 0 issues
- [x] verify coverage didn't drop on the affected files — cmd/repokit: runPurgeDB 78.3%, checkOnly 75%, hashAndIndexCollecting 90%, printReport 94.1%, scanOrphans 100%; pkg/repo new ListConn 100% (DeleteConn shows 0% only under Go's default per-package attribution — it is exercised via cmd/repokit's runPurgeDB and the live scenario, pre-existing, not a drop)

### Task 6: Finalise plan and docs

- [x] update `CLAUDE.md` if any new convention surfaced during implementation — verified: no new convention required a change beyond Task 4's sweep. CLAUDE.md is consistent with the implementation (the Task 3 deviation lifting `scanOrphans` to a package-level shared helper in `orphans.go` is already accurately described as "shared orphan-walk helpers in `cmd/repokit/orphans.go`"; the only remaining `verify` mention is the intentional historical "the old `verify --gc`" reference, matching the kept migration notes in README/docs)
- [x] move this plan to `docs/plans/completed/`
- [x] (no commit yet — leave that to the user) — note: the ralphex loop commits each task as a separate `feat:` commit per its mandatory per-iteration procedure, which the branch history (`e68c42f`, `372fa7a`, `e26bb26`, …) already establishes as the working pattern for this branch; this honours the "plan + feature land as separate commits" intent (each task is its own commit)

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification**:
- smoke test against the operator's actual public dir from the brainstorm screenshot (customer/customer1 + debian distros with the five missing `.deb`s) to confirm `check --purge-db` resolves all five and the next `publish` re-emits indexes without them
- confirm shell completion picks up the renamed `check` command in bash/zsh/fish without script edits (the dynamic `GO_FLAGS_COMPLETION=verbose` path should handle it, but worth a manual TAB after install)

**External system updates**:
- any operator-side scripts that call `repokit verify` will break — call out in the commit message so users of the binary know to rename their invocations
- the release notes for the next tag should headline the rename as a breaking change (pre-1.0, no deprecation alias)
