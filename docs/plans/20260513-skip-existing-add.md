# add --skip-existing flag for the `add` subcommand

## Overview

Add a new `--skip-existing` flag to `repokit add` that lets a batch invocation
skip (with a one-line stderr notice) `.deb` files whose
name/version/architecture (NVRA) is already present in the target
distro/suite/component, instead of aborting the whole batch with
`ErrDuplicate`. When the existing row's SHA256 differs from the
incoming `.deb`, emit a loud stderr warning so operators notice "same version,
different bytes" footguns without losing the rest of the batch.

The flag is mutually exclusive with `--replace`: the two encode opposite
intents ("overwrite collisions" vs. "leave collisions alone"). Unlike
`--replace`, `--skip-existing` imposes no batch-size limit — skipping never
writes or evicts pool bytes, so rollback safety is irrelevant.

## Context (from discovery)

- files involved:
  - `pkg/cli/add.go` — `AddCmd` struct, `run`, `processOne`, `printDuplicateDiff`
  - `pkg/cli/add_test.go` — existing test helpers (`initRepoForTest`,
    `fixtureDeb`, real-SQLite + real-pool fixtures) reused as-is
- patterns to mirror:
  - `printDuplicateDiff` (add.go:283) already demonstrates the `r.Get`
    pattern for fetching the existing row inside the surrounding transaction
  - The `--replace` validation block (`cmd.Replace && len(...) > 1`) at
    add.go:61 is the obvious sibling for the new mutual-exclusion check
  - Existing summary format on add.go:128–132 is the line to extend
- dependencies: none beyond the in-tree `pkg/repo`, `pkg/deb`, `pkg/pool`
  that `add.go` already uses

## Development Approach

- **testing approach**: Regular (code first, then tests). The change is
  small and localized; writing the helper and threading the sentinel is
  faster than building skeleton tests up-front, but every task still ends
  with its tests passing before moving on.
- complete each task fully before moving to the next
- make small, focused changes
- **every task includes new/updated tests for code changes in that task**
- **all tests must pass before starting next task** — no exceptions
- run `make test` and `make lint` after each task
- maintain backward compatibility: plain `add`, `--replace` single-deb, and
  `--replace` batch rejection must all still pass unchanged

## Testing Strategy

- **unit / CLI tests** in `pkg/cli/add_test.go`, real SQLite + real pool
  writes, `t.TempDir()`, `testify/assert` + `testify/require`. No new mocks.
- **no e2e tests** in this repo's sense; `integration_test.go` already
  exercises the full add→publish→verify flow and stays untouched.
- coverage target: every new code path (`--skip-existing` happy path,
  mismatch warning, mutual-exclusion error, summary suffix) is covered by
  at least one test case.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

The change is local to `pkg/cli/add.go`. The hook point is the existing
`ErrDuplicate` branch inside `processOne`: when `cmd.SkipExisting` is set,
delegate to a new `reportSkip` helper (sibling of `printDuplicateDiff`)
that prints either a plain skip line or a SHA256-mismatch warning, then
return an unexported `errSkipped` sentinel. The caller loop in `run`
treats `errSkipped` as "advance to next .deb, increment skip counter"
without recording written/backups/stale state. A mutual-exclusion check
in `run` rejects `--skip-existing --replace` at flag-validation time.
The trailing summary line conditionally appends `, skipped M`.

We deliberately do **not** pre-check existence with `r.Get`. The
transaction-level `r.Insert` is authoritative; a pre-check would duplicate
SQL and (in any future concurrent context) race.

## Technical Details

**New field on `AddCmd`:**

```go
SkipExisting bool `long:"skip-existing" description:"skip packages whose name/version/architecture is already present in the target (mutually exclusive with --replace)"`
```

**New unexported sentinel at package scope in `add.go`:**

```go
var errSkipped = errors.New("skipped")
```

**Mutual-exclusion check in `run`** (next to the existing `--replace` batch check):

```go
if cmd.SkipExisting && cmd.Replace {
    return errors.New("--skip-existing and --replace are mutually exclusive")
}
```

**Per-deb loop in `run`** — declare `var skipped int` near `written`, then:

```go
for _, p := range cmd.Args.Debs {
    diskPath, backup, stale, perr := cmd.processOne(ctx, c, r, tx, string(p))
    if perr != nil {
        if errors.Is(perr, errSkipped) {
            skipped++
            continue
        }
        return perr
    }
    written = append(written, diskPath)
    backups = append(backups, backup)
    if stale != "" {
        staleReplaced = append(staleReplaced, stale)
    }
}
```

**`processOne` `ErrDuplicate` branch** becomes:

```go
if errors.Is(ierr, repo.ErrDuplicate) {
    if cmd.SkipExisting {
        cmd.reportSkip(ctx, c, r, tx, &pkg)
        return "", "", "", errSkipped
    }
    cmd.printDuplicateDiff(ctx, c, r, tx, &pkg)
    return "", "", "", fmt.Errorf("%s_%s_%s already exists in %s/%s/%s (use --replace to overwrite)",
        pkg.Name, pkg.Version, pkg.Architecture,
        pkg.Distro, pkg.Suite, pkg.Component)
}
```

**New `reportSkip` helper** (sibling of `printDuplicateDiff`):

```go
func (cmd *AddCmd) reportSkip(ctx context.Context, c *Context, r *repo.Repo, tx *sql.Tx, pkg *repo.Package) {
    existing, err := r.Get(ctx, tx, pkg.Distro, pkg.Suite, pkg.Component,
        pkg.Architecture, pkg.Name, pkg.Version)
    if err == nil && existing != nil && existing.SHA256 != pkg.SHA256 {
        fmt.Fprintf(c.Stderr,
            "warning: skipping %s_%s_%s: NVRA matches but sha256 differs (existing=%s new=%s) — use --replace to overwrite\n",
            pkg.Name, pkg.Version, pkg.Architecture, existing.SHA256, pkg.SHA256)
        return
    }
    fmt.Fprintf(c.Stderr, "skipping %s_%s_%s: already present in %s/%s/%s\n",
        pkg.Name, pkg.Version, pkg.Architecture,
        pkg.Distro, pkg.Suite, pkg.Component)
}
```

**Updated summary** in `run`:

```go
if !c.Quiet {
    msg := fmt.Sprintf("staged %d packages", len(cmd.Args.Debs)-skipped)
    if skipped > 0 {
        msg += fmt.Sprintf(", skipped %d", skipped)
    }
    fmt.Fprintf(c.Stdout, "%s in %s/%s — run 'repokit publish --distro %s --suite %s' to release\n",
        msg, string(cmd.Distro), cmd.Suite, string(cmd.Distro), cmd.Suite)
}
```

Quiet (`c.Quiet`) suppresses the summary but **not** the per-skip stderr
lines or the mismatch warning — those are diagnostic and should always
fire.

The `err == nil && existing != nil` guard in `reportSkip` intentionally
degrades to the plain skip line on any `r.Get` anomaly (e.g.,
`ErrNotFound` from a concurrent delete): `r.Insert` has just proved the
NVRA exists inside the same transaction, so a read miss here is a benign
anomaly, not a hard error. Worth a one-line code comment so a future
reader doesn't try to surface the error.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all changes are in-repo
  (single source file + single test file) and verifiable with `make test`
  / `make lint`.
- **Post-Completion**: no manual steps. The flag has no migration impact,
  no config schema change, no shell-completion change.

## Implementation Steps

### Task 1: Add `--skip-existing` flag with mutual-exclusion guard

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/add_test.go`

- [ ] add `SkipExisting bool` field to `AddCmd` with the long flag and
      description spelled out in Technical Details
- [ ] add the `cmd.SkipExisting && cmd.Replace` mutual-exclusion check in
      `run`, sibling of the existing `--replace` batch check
- [ ] add a test case `skip-existing + replace rejected` in `add_test.go`:
      invoke `add --skip-existing --replace <one-deb>`, assert `ExitError`,
      stderr contains `--skip-existing and --replace are mutually exclusive`,
      no DB rows inserted, no pool files written
- [ ] run `make test` (race + coverage) — must pass before next task
- [ ] run `make lint` — must be clean

### Task 2: Wire `--skip-existing` end-to-end (sentinel, helper, run loop)

This task adds the helper + sentinel and immediately wires them in, so
`make lint` never sees a half-finished state with unused symbols. The
`errors.Is(perr, errSkipped)` check **must** live in the per-deb loop
body inside the `r.Tx` callback — returning `errSkipped` out of the
callback would roll back the entire transaction.

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/add_test.go`

- [ ] declare `var errSkipped = errors.New("skipped")` at package scope
      in `add.go`
- [ ] add the `reportSkip` method on `*AddCmd` per the Technical Details
      shape (SHA256-mismatch branch + plain-skip fallback), with a brief
      comment that the degraded-to-plain-line behaviour on `r.Get` failure
      is intentional
- [ ] in `processOne`, extend the `errors.Is(ierr, repo.ErrDuplicate)`
      branch to check `cmd.SkipExisting` and route through `reportSkip` +
      return `errSkipped`
- [ ] in `run`, declare `var skipped int` next to `written`, and in the
      per-deb loop inside `r.Tx` add the `errors.Is(perr, errSkipped)`
      branch that increments `skipped` and `continue`s (inside the loop
      body, not in the `r.Tx` return path)
- [ ] update the trailing summary in `run` to subtract `skipped` from the
      package count and conditionally append `, skipped M`
- [ ] add test case `skip-existing on identical re-add`: add the same .deb
      twice (second invocation with `--skip-existing`); assert ExitOK,
      stderr has `skipping <name>_<ver>_<arch>: already present in ...`,
      stdout has `staged 0 packages, skipped 1`, DB row count unchanged
- [ ] add test case `skip-existing no collisions`: invoke
      `add --skip-existing <new-deb>`; assert ExitOK, no `skipping` line
      on stderr, stdout summary has no `, skipped` suffix, row inserted
- [ ] add test case `skip-existing all skipped`: pre-populate two NVRAs,
      then run `add --skip-existing <both>` (batch of 2); assert ExitOK,
      stdout has `staged 0 packages, skipped 2`, no new files appear in
      the public dir (compare a `filepath.Walk` snapshot of `tmpPublic`
      before/after)
- [ ] add test case `skip-existing under --quiet`: pre-populate one NVRA,
      then run `add --skip-existing --quiet <existing-deb>`; assert ExitOK,
      stdout is empty (no summary), stderr still contains
      `skipping ...: already present`
- [ ] run `make test` — must pass before next task
- [ ] run `make lint` — must be clean

### Task 3: Test mixed-batch and SHA256-mismatch branches

**Files:**
- Modify: `pkg/cli/add_test.go`

- [ ] add test case `skip-existing mixed batch`: pre-populate one NVRA,
      then invoke `add --skip-existing <existing-deb> <new-deb-a> <new-deb-b>`
      (batch of 3, picking three distinct fixtures from
      `pkg/deb/testdata/` whose NVRAs are distinct — verify the
      directory has enough suitable fixtures before starting); assert
      ExitOK, exactly one `skipping` line on stderr, stdout summary
      `staged 2 packages, skipped 1`, two new pool files on disk for the
      non-skipped entries
- [ ] add test case `skip-existing on NVRA collision with different bytes`
      using the synthetic-row trick: open the SQLite DB directly via
      `repo.Open`, `Tx`-insert a `repo.Package` row with the real .deb's
      NVRA but a fake SHA256 and a placeholder `Filename` (a synthetic
      pool path is fine — no on-disk pool file is needed because
      `reportSkip` only reads the DB row's SHA256 and never touches the
      filesystem). Then invoke `add --skip-existing <real-deb>`; assert
      ExitOK, the `warning: skipping`/`NVRA matches but sha256 differs`
      line lands on **stderr** (not stdout), both `existing=` and `new=`
      hash fragments appear, DB row's SHA256 is still the fake one (not
      overwritten), and no new pool file is written for the synthetic
      Filename. Also run the same scenario with `--quiet` and assert the
      warning still fires on stderr.
- [ ] run `make test` — must pass before next task
- [ ] run `make lint` — must be clean

### Task 4: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented: flag exists,
      NVRA-match semantics, mismatch warning, mutual exclusion, no
      batch-size limit, summary suffix, quiet behaviour
- [ ] spot-check that existing regression coverage is intact: plain `add`
      duplicate still aborts with `already exists ... use --replace`;
      `--replace` single-deb still works; `--replace` >1 batch still
      rejected. Add minimal coverage for any branch that turns out to be
      uncovered
- [ ] run full test suite: `make test`
- [ ] confirm `make lint` is clean
- [ ] manually run `repokit add --help` and confirm the new flag's
      description renders correctly

### Task 5: [Final] Update documentation and move plan
- [ ] update `CLAUDE.md` only if a new convention surfaced during
      implementation (likely not — the change is local to existing
      patterns)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*No manual or external-system steps. The flag is additive and has no
deployment, migration, or downstream-consumer impact.*
