# Namespaced (Slashed) Distro Names

## Overview

Allow one-level namespaced distro names in `config.yaml` and on the `--distro`
flag — e.g. `customer/acme` alongside plain names like `debian`, `ubuntu`. A
namespaced distro produces a nested subtree under the public dir:
`<publicDir>/customer/acme/{pool,dists}/...`. This brings third-party /
per-customer Debian repos under repokit management without a parallel tool or
schema overhaul.

**Problem being solved.** Today distro names are restricted to `[a-zA-Z0-9-]+`
and validated identically as a single path segment. Customer repos at
`$PUBLIC/customer/$CUSTOMER_NAME` therefore have to be managed outside repokit.
The on-disk layout, DB schema, and signing model all already accommodate the
nested form — only the validators and the suite-key split need to learn about
slashes.

**Depth limit.** Exactly one optional `/`. A distro name is either a plain
segment (`[a-zA-Z0-9-]+`, no leading `-`) or two such segments joined by a
single `/`. Depth ≥ 3 is rejected. This keeps `splitSuiteKey`, Release-header
defaults, log fields, and operator mental model predictable.

**No migration.** No existing repokit-managed customer subtrees exist; the
operator's pre-existing parallel directories will be re-created cleanly via
`init` (with the new entry in `distros:`) then `add`. The DB `distro` column
is a string — existing rows keep their plain names, new rows store the
slashed form unchanged.

## Context (from discovery)

- Files involved: `pkg/config/config.go`, `pkg/cli/root.go` plus its `*_test.go` sibling, `pkg/cli/integration_test.go`, `pkg/pool/pool_test.go`, `CLAUDE.md`, `README.md`.
- Disk layer is already slash-agnostic: `pool.DiskPath(distro, filename)` is `distro + "/" + filename`, `filepath.Join(publicDir, distro, ...)` treats the embedded `/` as a separator, and `pool.SafeJoin` rejects `..` traversal on the combined path. No changes in `pkg/pool/pool.go`, `pkg/cli/add.go`, `pkg/cli/publish.go`, or `pkg/index/`.
- Existing validator templates: `validateDistroName` and `validateSuiteHeaders` in `pkg/config/config.go`; `validateScopeName`/`requireScopeName` in `pkg/cli/root.go`.
- Suite-config key today: `<distro>/<suite>` parsed by first-`/` split in `splitSuiteKey`. Needs to switch to last-`/` split so `customer/acme/stable` lands as (`customer/acme`, `stable`).
- Release-header defaults in `defaultSuite` (`pkg/config/config.go`): `Origin`/`Label` default to the distro name verbatim. For slashed distros the default becomes the full slashed name (e.g. `Origin: customer/acme`); operator can override per-suite in `config.suites`.
- Prefix-conflict hazard: `filesystemScopes` in `pkg/cli/publish.go` walks `<publicDir>/<distro>/dists/`. If `distros: [customer, customer/acme]` were both configured, the `customer/` directory would be claimed by both. Rejected at config load.

## Development Approach

- **Testing approach**: Regular (code first, then tests in the same task; matches the established `pkg/cli/*.go` + `*_test.go` sibling pattern and the prior `20260512-explicit-distros.md` plan).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
  - Tests are not optional — they are a required part of the checklist.
  - Write unit tests for new functions/methods.
  - Write unit tests for modified functions/methods.
  - Add new test cases for new code paths.
  - Update existing test cases if behavior changes.
  - Tests cover both success and error scenarios.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation**.
- Run `make test` after each change.
- Maintain backward compatibility: plain distro names keep behaving identically.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above).
- **Integration tests**: `pkg/cli/integration_test.go` gets one new end-to-end case for a namespaced distro (init → add → publish → verify on-disk layout and Release-header contents). No e2e UI tests in this repo.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## Solution Overview

Three small surgical changes, each isolated to its own file/package:

1. **Config validator** — extend `validateDistroName` (rename and export as `ValidateDistroName`) to accept one-level namespaced form, change `splitSuiteKey` to last-`/` split, add a prefix-conflict rule in `Load`. Everything downstream in `pkg/config` (`SuiteOrDefault`, `defaultSuite`, `applyDefaults`) already works unchanged because it operates on opaque strings.
2. **CLI validator split** — introduce `validateDistroScope`/`requireDistroScope` in `pkg/cli/root.go` that wrap the exported `config.ValidateDistroName` (plus the empty-string check, plus the `--<field>` quoting in error messages). Re-point every `--distro` call site to the new helpers; suite/component/arch keep slash-rejecting behavior. **Critically, this includes `dbScopes` in `pkg/cli/publish.go:515`**, which currently silently filters out DB rows whose distro contains a `/` — without repointing it, unscoped `publish` would never emit a namespaced distro's indexes. Update `--distro` flag help text on each subcommand struct in the same task.
3. **Tests + docs** — config tests for the new validator/split/conflict cases; CLI tests for the new distro-scope validator plus per-call-site coverage; one integration test exercising **unscoped** end-to-end namespaced-distro publish (specifically to exercise `dbScopes`); pool test for namespaced `DiskPath` + `SafeJoin` traversal guard; one-paragraph addition to CLAUDE.md Architecture and a README.md example.

**Scope decision — `list` and `search` are intentionally NOT touched.** Both commands today pass `cmd.Distro` directly to `repo.Filter` with no scope-name validation; they are read-only DB filters that never reach the filesystem. Adding new validation to them would be a separate behavior change and is out of scope for this plan.

## Technical Details

**`validateDistroName` — new rule.**
- If the name contains no `/`: validate as a single plain segment (current logic).
- If it contains exactly one `/`: split, validate both halves as plain segments via a shared `validateDistroSegment` helper, reject empty halves.
- If it contains ≥ 2 `/`: reject with "must be a single segment or two segments separated by '/'".
- Reject `.` / `..` as either segment (caught by the segment alphabet anyway, but make the message explicit if practical).

**`splitSuiteKey` — last-`/` split.**
- `strings.LastIndex(key, "/")` → split into `left`, `right`.
- `right` must be a plain (non-slashed) segment; `left` must pass `validateDistroName` (plain or namespaced).
- Empty `left` or `right` → reject. `customer//stable` naturally falls out (left=`customer/`, fails segment check on trailing empty).

**`Load` — prefix-conflict check.**
- After building the `distros` set, iterate over namespaced entries (`a/b`) and reject if the plain-segment prefix `a` is also a top-level entry in `distros`. Error message names both offending entries.

**`validateDistroScope` — CLI flag validator.**
- Same alphabet rules as `validateDistroName`, called from `add`, `delete`, `publish`, `verify`, `list`, `search`, plus any other `--distro` consumer.
- `requireDistroScope` wraps with the empty-string check (parallel to `requireScopeName`).
- `validateScopeName`/`requireScopeName` keep their current slash-rejecting bodies — used only for `--suite`, `--component`, `--arch`.

**Defaults for slashed distros.**
- `defaultSuite(distro, suite)` returns `Origin: distro` / `Label: distro` verbatim — so `customer/acme/stable` produces `Origin: customer/acme`. Per-suite overrides in `config.suites["customer/acme/stable"]` continue to work.

**Disk layout (no code change, recorded for clarity).**
- `<publicDir>/customer/acme/pool/<suite>/<comp>/<letter>/<src>/<file>.deb`
- `<publicDir>/customer/acme/dists/<suite>/{InRelease,Release,Release.gpg,...}`
- Staging dirs sit at the suite level (`<publicDir>/customer/acme/dists/<suite>.new/`), unaffected by the namespaced parent.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs — all in-repo.
- **Post-Completion** (no checkboxes): operator action of populating `config.yaml` with their actual customer entries and rerunning `add` against existing `.deb` files — outside the scope of this plan.

## Implementation Steps

### Task 1: Export `ValidateDistroName` with namespaced form and switch `splitSuiteKey` to last-`/` split

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [ ] extract a `validateDistroSegment(name string) error` (unexported) helper holding the current per-segment alphabet rules; used by both validation paths.
- [ ] rename `validateDistroName` to **exported** `ValidateDistroName` so `pkg/cli` can call it (it already imports `pkg/config`). Update internal callers in `pkg/config`.
- [ ] rewrite `ValidateDistroName` to accept either a single plain segment or exactly two plain segments joined by one `/`; reject depth ≥ 3, empty halves, and dot segments.
- [ ] change `splitSuiteKey` to use `strings.LastIndex(key, "/")` so namespaced distros parse correctly; keep the "both halves non-empty, right half no `/`" invariant.
- [ ] add table-driven cases to `TestValidateDistroName` for: valid `customer/acme`; invalid `customer/acme/extra`, `/acme`, `customer/`, `customer//acme`, `customer/.`, `customer/..`, `customer/-acme`, `-customer/acme`.
- [ ] add suite-key test cases: positive `customer/acme/stable` → (`customer/acme`, `stable`); positive `SuiteOrDefault("customer/acme", "stable")` round-trip against a loaded config; negative `customer/acme/stable/extra` rejected by `Load` (rejection should come from the distro validator on the left half — assert the error message mentions `customer/acme/stable`).
- [ ] run `go test ./pkg/config/... -race -count=1` — must pass before next task.

### Task 2: Add prefix-conflict check in `config.Load`

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [ ] after the duplicate-distro check in `Load`, iterate the configured `Distros` and reject any namespaced entry `a/b` whose first segment `a` is also present as a plain entry in the full distros set. Check is set-based so YAML ordering is irrelevant.
- [ ] error message names both offending entries (`customer` and `customer/acme`) so the operator can fix `config.yaml` in one edit.
- [ ] test: `distros: [customer, customer/acme]` → load fails with an error mentioning both entries.
- [ ] test: `distros: [customer/acme, customer]` (reversed YAML order) → same rejection, same error substring, confirming order-independence.
- [ ] test: `distros: [customer/acme, customer/beta]` (two siblings under same prefix, no plain `customer`) → loads cleanly.
- [ ] run `go test ./pkg/config/... -race -count=1` — must pass before next task.

### Task 3: Introduce `validateDistroScope` / `requireDistroScope` and repoint every distro call site

**Files:**
- Modify: `pkg/cli/root.go`
- Modify: `pkg/cli/root_test.go`
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/delete.go`
- Modify: `pkg/cli/info.go`
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/verify.go`

**Helper additions in `pkg/cli/root.go`:**

- [ ] add `validateDistroScope(val string) error` that calls `config.ValidateDistroName(val)` (exported in Task 1) and re-wraps the error to include `--distro` quoting consistent with `validateScopeName`'s error style.
- [ ] add `requireDistroScope(val string) error` that adds the empty-string check (parallel to `requireScopeName`).
- [ ] keep `validateScopeName`/`requireScopeName` bodies unchanged — they continue to reject `/` for suite/component/arch.
- [ ] update `--distro` flag `description:` tags on each subcommand's options struct (`AddCmd`, `DeleteCmd`, `PublishCmd`, `VerifyCmd`, `InfoCmd`) to mention the namespaced form, e.g. `"distro name (plain like 'debian' or namespaced like 'customer/acme'; must be declared in config.distros)"`. `ListCmd` / `SearchCmd` `--distro` description may also be updated for consistency but no validation is added (see Solution Overview).

**Call sites to repoint (enumerated from `grep -n 'validateScopeName\|requireScopeName' pkg/cli/*.go | grep -v _test.go`):**

- [ ] `pkg/cli/add.go:50` — inside the `for field, val := range map{fieldDistro: cmd.Distro, fieldSuite: cmd.Suite, "component": cmd.Component}` loop, special-case `field == fieldDistro` to call `requireDistroScope(val)`; other fields keep `requireScopeName(field, val)`. (Same pattern in delete and info — see below.)
- [ ] `pkg/cli/delete.go:87` — same special-case in the `requireScopeName` map loop.
- [ ] `pkg/cli/info.go:37` — same special-case in the `validateScopeName` map loop (info uses validate, not require, because all its scope fields are optional).
- [ ] `pkg/cli/publish.go:57` — replace `validateScopeName(fieldDistro, cmd.Distro)` with `validateDistroScope(cmd.Distro)`.
- [ ] `pkg/cli/publish.go:515` — **critical**: replace `validateScopeName(fieldDistro, sc.distro)` inside `dbScopes` with `validateDistroScope(sc.distro)`. Without this, no-arg `publish` silently skips every namespaced-distro row in the packages table (the `continue` on line 516 has no log line). Suite check on line 518 stays.
- [ ] `pkg/cli/verify.go:83` — replace `validateScopeName(fieldDistro, cmd.Distro)` with `validateDistroScope(cmd.Distro)`. Empty `cmd.Distro` is fine — `validateDistroScope` accepts empty just like `validateScopeName` does.

**Tests in `pkg/cli/root_test.go`:**

- [ ] `TestValidateDistroScope` table-driven cases parallel to `TestValidateScopeName`: positive plain (`debian`), positive namespaced (`customer/acme`), negative deeper (`customer/acme/extra`), negative leading-slash (`/acme`), negative trailing-slash (`customer/`), negative empty-middle (`customer//acme`), negative leading-dash (`-customer/acme`), negative dot segment (`customer/.`, `customer/..`).
- [ ] `TestRequireDistroScope` confirms empty-string rejection.
- [ ] verify the existing `TestValidateScopeName` cases still reject `/` (unchanged semantics for suite/component/arch).

**Per-call-site sanity tests (added to each command's existing `*_test.go`):**

- [ ] `add_test.go`: existing add tests get a fixture using `distros: [customer/acme]` and an `add --distro customer/acme --suite stable` invocation — assert it succeeds through `requireDistroScope` and `requireConfiguredDistro`.
- [ ] `publish_test.go`: a unit test that inserts a packages-table row directly via `repo` with `distro=customer/acme` and confirms it appears in `dbScopes` output (the bug fix from line 515). Cover both: (a) a row that should appear, (b) a row with `distro=foo/bar/baz` that should still be filtered out by `validateDistroScope`.
- [ ] `verify_test.go`: `verify --distro customer/acme` (with the distro configured) reaches `requireConfiguredDistro` instead of being rejected by the scope validator.
- [ ] `info_test.go`: `info --distro customer/acme --suite stable some.deb` parses scope flags without rejection.
- [ ] `delete_test.go`: `delete --distro customer/acme --suite stable --package foo` parses scope flags without rejection.

- [ ] run `go test ./pkg/cli/... -race -count=1` — must pass before next task.

### Task 4: End-to-end integration test for a namespaced distro

**Files:**
- Modify: `pkg/cli/integration_test.go`

- [ ] add `TestIntegration_NamespacedDistro` (or fold a sub-test into the existing integration suite, matching the file's style) that:
  - inits a repo with `distros: [customer/acme]` and one suite entry `customer/acme/stable` in `config.suites` (or relies on `defaultSuite`);
  - adds a tiny synthesized `.deb` via `add --distro customer/acme --suite stable` (reuse the fixture/helper the existing tests use);
  - **runs `publish` with no `--distro` / `--suite` flags** — this is mandatory so the `dbScopes` path is what selects the scope. A scoped `publish --distro customer/acme --suite stable` would bypass `dbScopes` entirely and not catch the bug fixed in Task 3;
  - asserts the pool file lives at `<publicDir>/customer/acme/pool/<suite>/<comp>/<letter>/<src>/<file>.deb`;
  - asserts the dists tree exists at `<publicDir>/customer/acme/dists/stable/{InRelease,Release}`;
  - reads `Release` and asserts all five header defaults from `defaultSuite`: `Origin: customer/acme`, `Label: customer/acme`, `Suite: stable`, `Codename: stable`, `Description: customer/acme stable`;
  - asserts the `Packages` files inside `dists/stable/<comp>/binary-<arch>/` reference the pool file with the apt-relative form (no leading `customer/acme/` — that segment comes from the URI base, per `pool.Filename`'s contract);
  - runs `verify` (no flags) and asserts it exits 0.
- [ ] add a negative case: `add --distro foo/bar` where `foo/bar` is not in `config.distros` fails with the existing `requireConfiguredDistro` error.
- [ ] run `go test ./pkg/cli/... -race -count=1` — must pass before next task.

### Task 5: Extend `pkg/pool` test for namespaced DiskPath and SafeJoin

**Files:**
- Modify: `pkg/pool/pool_test.go`

- [ ] extend the existing "different distros yield different disk paths" sub-test with a namespaced case (`DiskPath("customer/acme", filename)` produces `customer/acme/<filename>`).
- [ ] add a SafeJoin positive case: `<publicDir>/customer/acme/pool/...` resolves inside the public root, no rejection.
- [ ] add a SafeJoin negative case: a malformed namespaced distro like `customer/..` (or a `..` segment injected via `filename`) is rejected by `SafeJoin`. The guard is what makes the slash-agnostic design safe, so the test must exist.
- [ ] run `go test ./pkg/pool/... -race -count=1` — must pass before next task.

### Task 6: Update docs

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] add one sentence to the `Context.Config` / `Config.Distros` paragraph in CLAUDE.md's Architecture section noting that distros may be plain or one-level namespaced (`customer/acme`), and that plain-prefix conflicts (`[customer, customer/acme]`) are rejected at config load.
- [ ] add a `customer/<name>` example to the `distros:` block in README.md.
- [ ] run `make test && make lint` — must pass before final task.

### Task 7: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented.
- [ ] verify edge cases handled (depth ≥ 3, empty segments, prefix conflicts in both orderings, dot segments, `dbScopes` filter accepts slashed distros).
- [ ] run `make test` — all packages green with `-race`.
- [ ] run `make lint` — no new findings.
- [ ] manually inspect the generated `Release` file in the integration test artifact to confirm all five header defaults (`Origin`, `Label`, `Suite`, `Codename`, `Description`) are emitted verbatim.

### Task 8: [Final] Close out the plan

- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Operator follow-up** (outside this plan's scope):
- Operator edits `config.yaml` to add their actual customer entries under `distros:` (e.g. `customer/cust1`, `customer/cust2`).
- Operator runs `repokit add` against existing customer `.deb` files (or rebuilds the customer subtrees fresh).
- No deployment / CI changes required — pure library/CLI change.
