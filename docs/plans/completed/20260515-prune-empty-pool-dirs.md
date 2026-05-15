# Prune empty pool directories after package removal

## Overview

When `repokit delete` removes the last `.deb` from a pool subtree, or when
`repokit verify --gc` sweeps the last orphan from one, the parent directories
`<distro>/pool/<suite>/<comp>/<letter>/<src>/` are left behind empty:

```
tree /u01/packages/ubuntu/pool/resolute-devel
└── main
    └── libn
        └── libnxmodbus
4 directories, 0 files
```

This is cosmetic — apt does not care about empty directories — but it leaks
state, accumulates over time, and surprises operators who expect `tree` /
`find` output to reflect what is actually in the repository.

The fix is a small helper, `pool.PruneEmptyParents`, called immediately after
every successful `pool.Delete`. It walks up from the removed file's parent
directory, `rmdir`-ing each empty ancestor, and stops at `<root>/<distro>/pool`
so the per-distro pool root and everything above it are never touched.

## Context (from discovery)

- `pkg/pool/pool.go` — pool helpers. `Delete` at L130 is the single unlink path
  used by both call sites; `DiskPath` (L52) composes the disk-relative form;
  `SafeJoin` (L150) is the existing traversal guard; `hasParentSegment` (L170)
  is the `..`-rejection helper to reuse; `poolDir` const (L28) defines the
  literal `"pool"` segment.
- `cmd/repokit/delete.go:140` — first call site, inside `runDelete`'s
  writer-locked loop. Pool errors after a committed DB delete keep the row
  removed and surface as recoverable orphans; the prune must follow that same
  posture (a prune failure must not abort the loop or reintroduce DB state).
- `cmd/repokit/verify.go:344-351` — `removeOrphans`, called from
  `verifyAndGC` (also writer-locked). Signature change needed: `stderr
  io.Writer` threaded through from `verifyAndGC` (which already has it).
- `pkg/pool/pool_test.go` — uses `testify` (`require`, `assert`) and
  per-`t.TempDir()` setup. Table-driven tests (`TestFilename`) and direct
  filesystem assertions (`TestSafeJoin`, etc.) are the existing style.
- `cmd/repokit/delete_test.go:119` — `TestDeleteAllVersions` already exercises
  the "remove every version" path; it's the natural place to add the
  cleanup assertion.
- `cmd/repokit/verify_test.go` — has existing `--gc` cases to extend.

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
- maintain backward compatibility

## Testing Strategy

- **unit tests**: required for every task (see Development Approach above).
  `pkg/pool/pool_test.go` gets direct coverage of `PruneEmptyParents`.
- **integration tests**: `cmd/repokit/delete_test.go` and
  `cmd/repokit/verify_test.go` get end-to-end assertions that the on-disk
  tree is gone after the last file leaves a subtree.
- **e2e tests**: project has no UI; the integration tests above are the
  equivalent.
- run `make test` (race + coverage) and `make lint` after each task.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

A single helper, `pool.PruneEmptyParents(root, distro, relPath string) error`,
is added next to `pool.Delete` in `pkg/pool/pool.go`. It takes the same
disk-relative path the caller already passes to `Delete` (the output of
`pool.DiskPath`) and walks up from `filepath.Dir(<absolute dest>)`, calling
`os.Remove` on each step. The walk terminates on three conditions:

1. The candidate directory equals `filepath.Join(root, distro, "pool")` — the
   stop boundary. The boundary itself is never removed.
2. `os.Remove` returns `ENOTEMPTY` (or its Windows equivalent) — a sibling
   file or directory exists; the walk has hit a still-occupied ancestor and
   is done (no error returned).
3. `os.Remove` returns any other error — propagated to the caller.

Both call sites run inside a transaction that holds SQLite's single
writer slot (`delete` and `verify --gc` via `WithWriterLock` / `BEGIN
IMMEDIATE`; `add` via `r.Tx` whose first INSERT upgrades the tx). SQLite
permits only one writer at a time, so a concurrent `add`'s `MkdirAllSync`
+ `Rename` into the same shard directory cannot interleave with the
prune. No new locking is needed.

Prune errors at the call sites are non-fatal: the file is gone, the DB row is
gone, an extant empty directory is cosmetic. Both `delete` and `verify --gc`
log a `prune: <err>` warning to `c.Stderr` and continue the batch.

## Technical Details

### `PruneEmptyParents` contract

- Signature: `func PruneEmptyParents(root, distro, relPath string) error`.
- `relPath` is the disk-relative form: `<distro>/pool/<suite>/<comp>/<letter>/<src>/<file>.deb`,
  i.e. the same string passed to `Delete`. The function does NOT itself
  delete the file at `relPath`; it walks up from `filepath.Dir(relPath)`.
- Rejects `..` segments in `relPath` using `hasParentSegment` (already in
  `pool.go`) — a corrupted DB row must not be able to redirect the climb.
- Computes stop boundary as `filepath.Join(root, distro, poolDir)` (using
  the existing `poolDir` const).
- Loop body:
  - if candidate equals or is shallower than stop boundary → return nil
  - `err := os.Remove(candidate)`
  - if err is nil → ascend (`candidate = filepath.Dir(candidate)`)
  - if `errors.Is(err, syscall.ENOTEMPTY)` (with Windows mapping —
    `syscall.ENOTEMPTY` exists on Windows in Go's syscall package and maps
    to `ERROR_DIR_NOT_EMPTY`) → return nil (normal termination)
  - if `errors.Is(err, fs.ErrNotExist)` → already gone, ascend (idempotent)
  - else → return wrapped error
- Doc comment explains the stop boundary AND the `ENOTEMPTY`-is-normal
  rule (the WHY is non-obvious — a future reader could "fix" the error path
  and make every batch noisy).

### Call site changes

`cmd/repokit/delete.go:runDelete`:

```go
for i, p := range deleted {
    rel := pool.DiskPath(p.Distro, p.Filename)
    if err := pool.Delete(c.PublicDir, rel); err != nil {
        poolErr = err
        poolErrIdx = i
        return nil
    }
    if err := pool.PruneEmptyParents(c.PublicDir, p.Distro, rel); err != nil {
        fmt.Fprintf(c.Stderr, "prune: %v\n", err)
    }
}
```

`cmd/repokit/verify.go:removeOrphans` — add `stderr io.Writer` and a
`distros []string` parameter so the helper can recover the distro segment
from the orphan path. `strings.Cut(p, "/")` would be wrong: namespaced
distros (`customer/acme`) have a `/` *inside* the distro segment, so the
first-`/` split would yield `"customer"` and the stop boundary would
mis-anchor (`<publicDir>/customer/pool` instead of
`<publicDir>/customer/acme/pool`), letting the climb potentially remove
`customer/acme/pool/` itself.

Instead, match the orphan path against the configured distros list and
pick the longest prefix that is followed by `/pool/`:

```go
func removeOrphans(publicDir string, orphans []string, distros []string, stderr io.Writer) error {
    for _, p := range orphans {
        if err := pool.Delete(publicDir, p); err != nil {
            return fmt.Errorf("removing orphan %s: %w", p, err)
        }
        distro := matchOrphanDistro(p, distros)
        if distro == "" {
            continue // unknown layout — be conservative, skip prune
        }
        if err := pool.PruneEmptyParents(publicDir, distro, p); err != nil {
            fmt.Fprintf(stderr, "prune: %v\n", err)
        }
    }
    return nil
}

// matchOrphanDistro returns the configured distro whose <distro>/pool/
// prefix matches p, or "" if none does. Picks the longest match so a
// plain "customer" never shadows a namespaced "customer/acme" (the
// config loader already rejects that collision, but defending in depth).
func matchOrphanDistro(p string, distros []string) string {
    best := ""
    for _, d := range distros {
        prefix := d + "/" + poolDirName + "/"
        if strings.HasPrefix(p, prefix) && len(d) > len(best) {
            best = d
        }
    }
    return best
}
```

Caller `verifyAndGC` passes `stderr` and the already-in-scope `distros`
slice.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all code, signature, and test
  changes in this repo.
- **Post-Completion** (no checkboxes): nothing external — this is a
  self-contained behavior change. Operators with existing empty trees can
  remove them by hand once or wait for the next `verify --gc` to no-op
  (the helper does nothing if there is nothing left to delete at that path,
  so existing empties from prior runs are NOT auto-cleaned; see Out of
  Scope).

## Implementation Steps

### Task 1: Add `PruneEmptyParents` helper in `pkg/pool`

**Files:**
- Modify: `pkg/pool/pool.go`
- Modify: `pkg/pool/pool_test.go`

- [x] add `PruneEmptyParents(root, distro, relPath string) error` to `pkg/pool/pool.go`
- [x] reuse `hasParentSegment` for `..` rejection (defense in depth: the path is not dereferenced, but a `..` would misanchor the climb's starting point)
- [x] compute stop boundary as `filepath.Join(rootAbs, distro, poolDir)`; never remove it
- [x] use `errors.Is(err, syscall.ENOTEMPTY)` to detect normal termination (works on Linux/macOS/Windows — Go's `syscall` maps Windows `ERROR_DIR_NOT_EMPTY` to the same sentinel); `errors.Is(err, fs.ErrNotExist)` to ascend through already-removed dirs (idempotency)
- [x] add a doc comment explaining (a) the stop boundary, (b) why `ENOTEMPTY` is treated as success, (c) why the `..` guard is needed even though `relPath` is not dereferenced
- [x] write unit test: full empty chain pruned up to (but not including) stop boundary
- [x] write unit test: stop boundary directory itself is preserved when its children are pruned
- [x] write unit test: non-empty sibling stops the climb at the right level (deeper dirs gone, occupied ancestor remains)
- [x] write unit test: parent of `relPath` IS the stop boundary (file lives directly under `<distro>/pool/`) — helper is a no-op, boundary preserved
- [x] write unit test: stop boundary directory does not exist yet — helper returns nil cleanly
- [x] write unit test: `..` segment in `relPath` rejected with error
- [x] write unit test: missing parent dir is a no-op (no error)
- [x] write unit test: idempotent — second call after the chain is gone returns nil
- [x] write unit test: namespaced distro (`customer/acme`) — chain pruned correctly, `<publicDir>/customer/acme/pool/` preserved as stop boundary
- [x] run `go test ./pkg/pool/... -race -count=1` — must pass before next task

### Task 2: Wire prune into `delete`

**Files:**
- Modify: `cmd/repokit/delete.go`
- Modify: `cmd/repokit/delete_test.go`

- [x] in `runDelete`'s loop, after a successful `pool.Delete`, call `pool.PruneEmptyParents(c.PublicDir, p.Distro, pool.DiskPath(p.Distro, p.Filename))`
- [x] on prune error, `fmt.Fprintf(c.Stderr, "prune: %v\n", err)`; do NOT break the loop or set `poolErr`/`poolErrIdx`
- [x] extend `TestDeleteAllVersions` (or add a sibling case) asserting `<publicDir>/<distro>/pool/<suite>` no longer exists on disk after the delete, AND `<publicDir>/<distro>/pool/` itself remains
- [x] add a test case: delete that leaves some packages behind in the same suite — assert the still-populated ancestor dirs are preserved (`<src>/` for a different source under same `<letter>` stays, `<suite>/` stays)
- [x] add a test case: namespaced distro — `--distro customer/acme --suite <s> --all-versions ...` removes everything; assert `<publicDir>/customer/acme/pool/<suite>` is gone and `<publicDir>/customer/acme/pool/` remains (regression guard for the namespaced-distro fix in `removeOrphans`, and confirms the helper handles namespaced distros end-to-end)
- [x] run `go test ./cmd/repokit/... -race -count=1 -run TestDelete` — must pass before next task

> Note: the prune-error stderr path is covered by Task 1's `..`-rejection unit test (the only error path that surfaces a non-nil return). A read-only-parent integration test is brittle across platforms and adds no coverage beyond the unit test, so it is intentionally omitted.

### Task 3: Wire prune into `verify --gc`

**Files:**
- Modify: `cmd/repokit/verify.go`
- Modify: `cmd/repokit/verify_test.go`

- [x] change `removeOrphans` signature to `removeOrphans(publicDir string, orphans []string, distros []string, stderr io.Writer) error`
- [x] add a `matchOrphanDistro` helper that picks the longest configured-distro prefix matching `<distro>/pool/...` — required because namespaced distros (`customer/acme`) contain a `/` and `strings.Cut(p, "/")` would mis-anchor the prune to `<publicDir>/customer/pool`
- [x] in `removeOrphans`, after each successful `pool.Delete`, look up the distro via `matchOrphanDistro` and call `pool.PruneEmptyParents(publicDir, distro, p)` (skip if no match, defensively)
- [x] on prune error, warn to stderr; continue
- [x] update the single caller `verifyAndGC` to pass `stderr` and `distros` (both already in scope)
- [x] extend a `--gc` test in `verify_test.go` where the orphan is the only file in its pool subtree — assert the subtree is gone after `--gc`
- [x] add a test case: orphan removed, sibling rows still keep their tree intact (regression guard against the helper climbing too far)
- [x] add a test case: namespaced distro orphan (`customer/acme/pool/<suite>/...`) — assert `matchOrphanDistro` picks `customer/acme`, the chain is pruned, and `customer/acme/pool/` is preserved
- [x] add a unit test for `matchOrphanDistro` directly: longest-prefix wins, no-match returns "", plain distro doesn't shadow a namespaced one
- [x] run `go test ./cmd/repokit/... -race -count=1 -run TestVerify` — must pass before next task

### Task 4: Verify acceptance criteria

- [x] reproduce the original scenario in an integration test or by hand: add a single package, delete it, assert `<publicDir>/<distro>/pool/<suite>` is gone (but `<publicDir>/<distro>/pool/` remains) — covered by `TestDeletePrunesEntireSuite` (delete_test.go:159) and `TestDeleteNamespacedDistroPrunes` (delete_test.go:183)
- [x] verify `add` after a full prune correctly recreates the shard dirs (`MkdirAllSync` already handles this; this is a smoke check, not a new test) — implicit: every existing add test runs against an empty `tmpPublic` with no pool shards, which is equivalent to the post-prune state; `MkdirAllSync` is exercised by every successful add
- [x] run `make test` — passed (all 7 packages OK, 83.2% coverage)
- [x] run `make lint` — passed (0 issues)
- [x] verify no unrelated test regressions — all packages remain green

### Task 5: [Final] Wrap up

- [x] update CLAUDE.md if any new pattern is worth documenting — no update needed; `PruneEmptyParents` is a localized helper that does not introduce a new architectural pattern or gotcha
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Operator note (informational, not a code task):** existing empty trees from
previous deletes are NOT auto-cleaned by this change — the helper only fires
when a `.deb` is removed at or below that subtree. Operators with leftover
empties can either remove them by hand (`find <publicDir>/<distro>/pool -type d -empty -delete`)
or wait for a future delete/gc in the same subtree to sweep them away.
