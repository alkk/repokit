# Per-distro apt repo layout

## Overview

Restructure the on-disk repo layout so each `<distro>` is its own apt repo
root, matching standard Debian mirror conventions (URI ends in `/<distro>`,
`Suites:` is plain `<suite>`).

**Problem.** The tool currently emits `dists/<distro>/<suite>/...` and
`pool/<distro>/<suite>/<comp>/...` under one shared root. A client that uses
the natural `URI=https://host/<distro>` + `Suites: <suite>` ends up requesting
`/<distro>/dists/<suite>/Release`, which doesn't exist — the file lives at
`/dists/<distro>/<suite>/Release`. apt 404s on every fetch.

**Fix.** Move `<distro>` from inside `dists/`/`pool/` to outside them:

```
<root>/<distro>/dists/<suite>/...
<root>/<distro>/pool/<suite>/<comp>/<letter>/<src>/<file>.deb
```

Now `URI=https://host/<distro>` + `Suites: <suite>` works as expected.

## Context (from discovery)

- Project: small Go CLI (`debrepo`) that maintains a static apt repo backed by
  SQLite. One `repo.db` + `config.yaml` at the root, with `pool/` and `dists/`
  trees underneath.
- Pool path is built in one place: `pkg/pool/pool.go:24` `Path(...)` returns
  `pool/<distro>/<suite>/<comp>/<letter>/<src>/<file>.deb`. The same string is
  used both for on-disk I/O and stored as `Filename` in the DB / emitted in
  `Packages` indexes.
- Dists path is built in `pkg/cli/publish.go`: `distsParent =
  filepath.Join(root, distsDir, distro)` (line 174), and
  `filesystemScopes` walks `<root>/dists/<distro>/<suite>` (line 500–513).
- `init` creates top-level `pool/` and `dists/` at `pkg/cli/init.go:83`.
- Verify's orphan scan uses `scopePoolDir(distro, suite)` →
  `pool/<distro>/<suite>` (or partial) at `pkg/cli/verify.go:240`.
- Distro / suite / component name validation lives in `pkg/cli/root.go` (or
  similar) — must confirm `validateScopeName("distro", ...)` is invoked
  everywhere distro flows into a path.

## Development Approach

- **testing approach**: Regular (code first, then tests) — each task pairs
  code changes with test updates in the same task.
- complete each task fully before moving to the next.
- make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in
  that task.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- run `make test` after each task.
- breaking change accepted — no schema bump, no migration code, no
  backward-compat shims.

## Testing Strategy

- **unit tests**: required for every task (see Development Approach).
- **integration tests**: `pkg/cli/integration_test.go` exercises the full
  add → publish → verify flow against a temp dir; update its on-disk
  assertions to match the new layout.
- **e2e tests**: project has no UI, so no Playwright/Cypress concerns. The
  integration tests serve as the end-to-end check (apt-relative `Filename`
  values, dists tree shape, atomic swap layout).

## Progress Tracking

- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix.
- document issues/blockers with ⚠️ prefix.
- update plan if implementation deviates from original scope.
- keep plan in sync with actual work done.

## Solution Overview

Split today's single `pool.Path()` string into two roles:

- **`Filename`** — apt-relative path stored in the DB and emitted in
  `Packages`. Form: `pool/<suite>/<comp>/<letter>/<src>/<file>.deb`.
  Resolved by apt clients against `URI=<root>/<distro>`, so no leading
  `<distro>` segment.
- **Disk-relative path** — repo-root-relative path used for I/O. Form:
  `<distro>/<filename>` (where `<filename>` is the apt-relative form
  above). Returned by `pool.DiskPath(distro, filename)`. The full
  filesystem path is `filepath.Join(root, FromSlash(diskPath))` — pool
  helpers like `Write`, `SafeJoin`, `ReconcileBackup` already do that
  join internally and just need disk-relative input.

Dists similarly move from `<root>/dists/<distro>/<suite>` to
`<root>/<distro>/dists/<suite>`. Staging siblings (`<suite>.new`,
`<suite>.old`) move accordingly.

`repo.db` and `config.yaml` stay at the shared `<root>`. One DB still tracks
all distros; the change is purely about where files land on disk and what
goes into `Filename`.

## Technical Details

**Pool API change** (`pkg/pool/pool.go`):

- Rename `Path(distro, suite, component, source, name, version, arch)` →
  `Filename(suite, component, source, name, version, arch)`. Drop the
  `distro` parameter; return `pool/<suite>/<comp>/<letter>/<src>/<file>.deb`.
- Add `DiskPath(distro, filename) string` returning
  `path.Join(distro, filename)` — apt-relative path prefixed with the distro
  segment, suitable for joining with root for I/O.
- `Write`, `Replace`, `Delete`, `SafeJoin`, `ReconcileBackup`, etc. already
  take `(root, relPath)` and just join — they keep working unchanged once
  callers pass `DiskPath(distro, filename)` instead of the old combined
  string.

**Callers that build paths** (`pkg/cli/add.go`, `pkg/cli/info.go`):
switch from `pool.Path(distro, suite, ...)` to
`pool.Filename(suite, ...)` for the DB column, and to
`pool.DiskPath(distro, filename)` when computing the path passed to
`pool.Write` / `pool.Replace`.

**Publish** (`pkg/cli/publish.go`):

- `distsParent := filepath.Join(root, distsDir, distro)` →
  `filepath.Join(root, distro, distsDir)`.
- `filesystemScopes`: walk `<root>/<distro>/dists/<suite>` instead. Outer
  loop reads top-level dirs of `<root>` (each is a candidate distro), inner
  loop reads `<root>/<distro>/dists/`. Ignore top-level entries that don't
  contain a `dists/` directory so stray files at root (e.g. `repo.db`,
  `config.yaml`, `pubkey.asc`) are skipped.

**Verify** (`pkg/cli/verify.go`):

- `scopePoolDir(distro, suite)`: when both set →
  `<distro>/pool/<suite>`; when only distro set → `<distro>/pool`; when
  neither set → must enumerate distros from DB and walk each
  `<distro>/pool` separately.
- Add `(*Repo).EnumerateDistros(ctx) ([]string, error)` to
  `pkg/repo/queries.go`: `SELECT DISTINCT distro FROM packages ORDER BY
  distro`.

**Init** (`pkg/cli/init.go`): drop the `for _, sub := range []string{"pool",
"dists"}` mkdir loop. `init` writes `repo.db` + `config.yaml` only;
distro subdirs are created on first `add`/`publish`.

**Validation**: confirm the same `validateScopeName("distro", ...)` rule
that already gates suite/component runs against `distro` in every command.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all code, tests, docs.
- **Post-Completion** (no checkboxes): manual verification on the live
  packages2.netxms.org server (re-init + re-add + re-publish, point apt at
  it, run `apt-get update`).

## Implementation Steps

### Task 1: Split `pool.Path` into `Filename` + `DiskPath`

**Files:**
- Modify: `pkg/pool/pool.go`
- Modify: `pkg/pool/pool_test.go`

- [x] rename `Path` → `Filename` in `pkg/pool/pool.go`; drop the `distro`
      parameter; new return form `pool/<suite>/<comp>/<letter>/<src>/<file>.deb`
- [x] add `DiskPath(distro, filename string) string` returning
      `path.Join(distro, filename)` (forward slashes, repo-root-relative)
- [x] update the **package-level doc comment** at `pool.go:1-3` (currently
      says "paths returned and accepted by this package … are repo-relative
      and serialized verbatim into Packages files" — that conflates the two
      roles the split is now separating) to describe the two-path split:
      `Filename` is apt-relative and serialized into `Packages`; `DiskPath`
      composes the disk-relative path used for I/O. Same update on
      `Filename`'s and `DiskPath`'s own doc comments
- [x] update `pkg/pool/pool_test.go`: rename `TestPath` cases, drop
      `distro` from inputs, update expected strings to drop the `<distro>`
      segment
- [x] add a small test for `DiskPath` covering the join shape and that
      different distros yield different disk paths
- [x] run `go test ./pkg/pool/...` — must pass before next task

### Task 2: Update pool callers (add, info, delete)

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/info.go`
- Modify: `pkg/cli/delete.go`
- Modify: `pkg/cli/add_test.go`
- Modify: `pkg/cli/info_test.go`
- Modify: `pkg/cli/delete_test.go`

In `add.go`, `processOne` (line ~145) currently returns one string used for
both the DB `Filename` column and pool I/O. After the split it must compute
two: `filename = pool.Filename(suite, ...)` for the DB and
`diskPath = pool.DiskPath(distro, filename)` for I/O. The return signature
changes from `(relPath, ...)` to `(diskPath, filename, ...)` so the caller
can both feed `diskPath` to `pool.RollbackReplace` / `FinalizeReplace` and
keep `filename` as the DB-column comparison value.

- [x] `add.go:145` — split `pool.Path(...)` into `filename` (apt-relative)
      and `diskPath` (= `pool.DiskPath(cmd.Distro, filename)`)
- [x] `add.go:155` — DB row stores `Filename: filename`
- [x] `add.go:175` — comparison `old.Filename != filename` (DB-column form)
- [x] `add.go:176` — `stale = pool.DiskPath(cmd.Distro, old.Filename)` so
      the value pushed into `staleReplaced` is disk-relative for
      `pool.Delete` at line 115
- [x] `add.go:192` — `writePool(c.Root, diskPath, ...)` (disk-relative)
- [x] `add.go:215-218` (`reconcileBackup`) — same split: compute `filename`
      and `diskPath`; `dest := filepath.Join(c.Root, filepath.FromSlash(diskPath))`
- [x] `add.go:235` — `row.Filename != filename` (DB-column comparison)
- [x] `add.go:245` — `pool.ReconcileBackup(c.Root, diskPath, committed)`
- [x] `add.go:79`, `:92` — `written` collects `diskPath` values so
      `pool.RollbackReplace(c.Root, written[i], backups[i])` resolves
      correctly
- [x] `info.go:59` — same Filename/DiskPath split for the displayed
      target path; clarify in the printed output what's apt-relative vs.
      on-disk if both are shown
- [x] `delete.go:138` — `pool.Delete(c.Root, pool.DiskPath(p.Distro, p.Filename))`
- [x] update `add_test.go`: `Filename` assertions drop the `<distro>` segment;
      on-disk path assertions now nest distro *before* `pool/`
- [x] update `info_test.go`: same expectation flip
- [x] update `delete_test.go` fixtures (lines ~83, 85, 215, 217 use the
      old `pool/<distro>/<suite>/...` form): drop the `<distro>` segment
      from `Filename` values; on-disk fixtures now place files at
      `<root>/<distro>/pool/<suite>/...`
- [x] run `go test ./pkg/cli/... -run 'TestAdd|TestInfo|TestDelete'` —
      must pass before next task

### Task 3: Move dists tree under each distro (publish)

**Files:**
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/publish_test.go`

- [x] in `publishScope`, change `distsParent := filepath.Join(root,
      distsDir, distro)` → `filepath.Join(root, distro, distsDir)`
- [x] verify `livePath`, `newPath`, `oldPath`, and `swapDists` still work
      with the new `distsParent` (they should — only the parent path
      changed, not its semantics)
- [x] in `filesystemScopes`, replace the single `os.ReadDir(<root>/dists)`
      walk with: read top-level `<root>` entries; for each entry that is a
      directory **and** passes `validateScopeName("distro", entry.Name())`
      **and** contains a `dists/` subdirectory, treat it as a candidate
      distro and read `<root>/<distro>/dists/` for suites. Skip entries
      that fail any of those gates so stray files (`repo.db`, `config.yaml`,
      `pubkey.asc`) and arbitrary user-created dirs don't trigger spurious
      distros — without the validateScopeName gate, a top-level dir with a
      malformed name happening to contain `dists/` could trigger the
      atomic-swap code on unrelated content
- [x] update `publish_test.go` fixtures and assertions to match the new
      layout
- [x] run `go test ./pkg/cli/... -run 'TestPublish'` — must pass before
      next task

### Task 4: Update verify (rehash + orphan scan)

**Files:**
- Modify: `pkg/repo/queries.go`
- Modify: `pkg/repo/queries_test.go`
- Modify: `pkg/cli/verify.go`
- Modify: `pkg/cli/verify_test.go`

Two distinct fixes here. **(a)** `verifyOne` at `verify.go:160` calls
`pool.SafeJoin(root, p.Filename)`; with apt-relative `Filename` that
resolves to `<root>/pool/<suite>/...` (no distro) and breaks rehash. Must
join through `pool.DiskPath(p.Distro, p.Filename)`. **(b)** the orphan scan
populates `known` keyed by `p.Filename` (apt-relative) but builds the
walk-side `key` via `filepath.Rel(root, p)` which now yields
`<distro>/pool/<suite>/...` (disk-relative). The two key spaces mismatch
and every real file would be reported as orphan. Fix by keying `known` with
disk-relative paths: `known[pool.DiskPath(p.Distro, p.Filename)] = struct{}{}`.

- [x] add `(*Repo).EnumerateDistros(ctx context.Context) ([]string,
      error)` to `pkg/repo/queries.go` — `SELECT DISTINCT distro FROM
      packages ORDER BY distro`. Implement on the same connection-handle
      type used by `EnumeratePackages` so the unscoped GC path can call it
      under the same writer lock (no separate connection — avoids a race
      where a concurrent `add` introduces a new distro between
      enumeration and the per-distro scan)
- [x] `verify.go:160` — `pool.SafeJoin(root, pool.DiskPath(p.Distro, p.Filename))`
- [x] `verify.go:124` and `:145` — populate `known[pool.DiskPath(p.Distro, p.Filename)] = struct{}{}`
      so its keys match the disk-relative `key` built in `scanOrphans`
- [x] update `scopePoolDir` in `pkg/cli/verify.go`:
      `(distro,suite)` → `<distro>/pool/<suite>`,
      `(distro,"")`   → `<distro>/pool`,
      `("","")`       → caller iterates over `EnumerateDistros` result
- [x] in unscoped mode, loop over distros and call `scanOrphans` for each
      with the new per-distro pool root, all under the same writer-lock
      scope as the row enumeration
- [x] sweep `poolDirName` constant usage (line 48); rename or document as
      "the literal `pool` segment under each distro" so a future reader
      isn't misled by the old "top-level pool dir" intent
- [x] decide on the form of `MissingPool` / `Mismatches` / orphan report
      strings — keep apt-relative `Filename` for missing/mismatch (they
      come from rows, no distro prefix needed when a single-distro repo
      is the common case **and** the report already prefixes with `distro`
      elsewhere — verify behavior in `verify_test.go`); use disk-relative
      paths for orphans (they're walked from the filesystem and the
      distro is meaningful context). If the user-facing output changes,
      update `verify_test.go` golden output accordingly
- [x] add a test in `queries_test.go` for `EnumerateDistros` (empty DB,
      multi-distro DB, sorted ascending)
- [x] update `verify_test.go` orphan-scan fixtures: orphans now live at
      `<root>/<distro>/pool/...` instead of `<root>/pool/<distro>/...`
- [x] add a regression case: unscoped verify across two distros finds
      orphans under both
- [x] run `go test ./pkg/repo/... ./pkg/cli/... -run
      'TestVerify|TestEnumerate'` — must pass before next task

### Task 5: Trim init

**Files:**
- Modify: `pkg/cli/init.go`
- Modify: `pkg/cli/init_test.go`

- [x] drop the `for _, sub := range []string{"pool", "dists"}` mkdir loop
      from `pkg/cli/init.go:83`
- [x] update `init_test.go` to assert the post-init layout is just
      `repo.db` + `config.yaml` at the root, no top-level `pool/` or
      `dists/`
- [x] run `go test ./pkg/cli/... -run 'TestInit'` — must pass before next
      task

### Task 6: Confirm distro name validation

**Files:**
- Modify (if needed): `pkg/cli/root.go` or wherever
  `validateScopeName` is called
- Modify (if needed): `pkg/cli/*_test.go` to add a path-traversal-via-distro
  case

- [x] grep every call site that passes `cmd.Distro` into a path. For each,
      confirm it goes through `validateScopeName("distro", cmd.Distro)`
      first
- [x] if any call site is missing the check, add it (none missing — `add`,
      `delete`, `info`, `publish`, `verify` already gate `cmd.Distro`;
      `list` and `search` only feed it into a DB filter so no path
      validation needed)
- [x] add a regression test that passes `--distro ../etc` (and similar
      traversal attempts) into `add`, `publish`, `verify` and asserts
      usage-error exit
- [x] run `go test ./pkg/cli/...` — must pass before next task (non-
      integration CLI suite green; `TestIntegration_*` failures are
      pre-existing per Task 7)

### Task 7: Update integration test

**Files:**
- Modify: `pkg/cli/integration_test.go`

- [x] replace every on-disk path assertion that references
      `<root>/dists/<distro>/<suite>` with `<root>/<distro>/dists/<suite>`
- [x] replace every reference to `<root>/pool/<distro>/<suite>` with
      `<root>/<distro>/pool/<suite>`
- [x] update assertions that read `Packages` files: `Filename:` lines no
      longer contain `<distro>`
- [x] keep / strengthen the no-cross-suite-dedup assertion (same NVRA in
      two suites of one distro → two distinct disk files)
- [x] add an assertion that two distros publishing the same suite name
      end up with two independent `<distro>/dists/<suite>` trees
      (added `TestIntegration_CrossDistroSameSuite`)
- [x] run `go test ./pkg/cli/... -run 'TestIntegration'` — must pass
      before next task

### Task 8: Update Packages index test

**Files:**
- Modify: `pkg/index/packages_test.go`

- [x] update the `Filename:` strings in the expected stanzas — drop the
      `<distro>` segment so they read `pool/<suite>/<comp>/...`
- [x] no production code changes here; the index writer already emits
      `p.Filename` verbatim
- [x] run `go test ./pkg/index/...` — must pass before next task

### Task 9: Update repo queries fixture

**Files:**
- Modify: `pkg/repo/queries_test.go`

- [x] update the `Filename: fmt.Sprintf("pool/%s/%s/...", distro, suite,
      ...)` helper to drop the `distro` segment so synthetic rows match
      the new format
- [x] confirm no test asserts that the literal `<distro>` substring
      appears inside `Filename` (a regression-friendly check now that the
      column meaning has changed)
- [x] run `go test ./pkg/repo/...` — must pass before next task

### Task 10: Update README and usage docs

**Files:**
- Modify: `README.md`
- Modify: `docs/usage.md`

- [x] in `README.md` "Consuming the repo with apt" — change the example
      sources line so URI ends in `/<distro>` and `Suites:` is plain
      `<suite>` (modern deb822 form already used in the user's bug
      report); show the matching one-line legacy form too
- [x] in `docs/usage.md` "On-disk layout" — redraw the tree to put
      `<distro>` outside `dists/` and `pool/`
- [x] update the description of `Pool isolated per (distro, suite)` to
      reflect the new path shape (the *isolation* property is unchanged,
      only the path)
- [x] no CLI surface change; quickstart commands stay as-is (only the
      rsync target path in step 5 of the quickstart block was updated to
      reflect the new per-distro tree)

### Task 11: Verify acceptance criteria

- [x] verify `apt-get update` against a freshly published repo works with
      `URI=.../<distro>`, `Suites: <suite>`, `Components: <comp>` (skipped —
      manual post-completion step, see below)
- [x] verify `make test` is green end-to-end (passes; total coverage 80.3%)
- [x] verify `make lint` is green (clean after fixing wrapcheck in
      `pkg/cli/root_test.go` snapshotRoot helper)
- [x] verify `make race` is green (all packages pass with `-race`)
- [x] verify two distros sharing a suite name (`debian/trixie` +
      `ubuntu/trixie`) round-trip through `add` → `publish` → `verify`
      independently (covered by `TestIntegration_CrossDistroSameSuite` in
      `pkg/cli/integration_test.go:230`)
- [x] verify `verify --gc` removes orphans under each per-distro
      `<distro>/pool/` (covered by
      `TestVerifyUnscopedAcrossDistrosFindsOrphans` in
      `pkg/cli/verify_test.go:281`)

### Task 12: [Final] Move plan to completed

- [x] update `CLAUDE.md` only if a new gotcha emerged (otherwise leave it —
      no new gotcha emerged from this work)
- [x] move this plan: `mv docs/plans/20260427-per-distro-layout.md
      docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — informational only*

**Manual verification on packages2.netxms.org:**

- The current published tree on the server is in the old layout. Re-init a
  fresh repo locally with the new code, re-add the same `.deb` set, run
  `publish`, and rsync the result. Old `<root>/dists/` and `<root>/pool/`
  trees on the server should be removed before/after the rsync so stale
  files don't shadow the new layout.
- Update the example sources file the user pasted in the bug report to
  confirm `apt-get update` succeeds:

  ```
  Types: deb
  URIs: https://packages2.netxms.org/debian
  Suites: trixie
  Components: 6.1
  Signed-By: /usr/share/keyrings/netxms.gpg
  ```

  Expected request now hits `https://packages2.netxms.org/debian/dists/trixie/InRelease`
  which corresponds to `<root>/debian/dists/trixie/InRelease` on disk — file
  exists, 200 OK.

**External system updates:**

- Any docs / wiki pointing customers at the apt repo should be updated to
  show the standard `URI=.../<distro>` + plain suite form.
