# Explicit Managed Distros in Config

## Overview

Add a mandatory `distros: []string` field to `config.yaml` that lists the distros
repokit is allowed to manage. All write commands (`add`, `delete`, `publish`,
`verify`) reject `--distro` values not in this list. The on-disk distro
discovery in `pkg/cli/verify.go` (`unionWithDiskDistros`) is removed.

**Problem being fixed.** `verify --gc` today computes its orphan-walk scope by
unioning DB-known distros with on-disk dirs that look like a distro (pass
`validateScopeName`) and contain a `pool/` subdirectory. In real deployments
the public dir holds unrelated content — e.g. `<publicDir>/raspbian/pool/` and
`<publicDir>/test/pool/` managed by other tools. An unscoped
`repokit verify --gc` would walk those trees, classify every file as an orphan
(no matching DB row), and delete them. Making the distro list explicit closes
the hole at its source: repokit never touches a distro the operator didn't
declare.

**Why now.** No production deployments yet, so the hard cutover is cheap.
Slash-in-distro support (for future customer trees like `customer/cust1`) is
deferred to a separate follow-up task — it has its own design questions
(suite-key syntax, multi-segment path handling) that are best resolved after
the explicit-list groundwork is in.

## Context (from discovery)

- Files involved: `pkg/config/config.go`, `pkg/cli/{root,add,delete,publish,verify,init}.go` and their `*_test.go` siblings, plus `pkg/cli/integration_test.go` and `integration_realpkg_test.go`.
- Existing patterns: subcommand dispatch via `jessevdk/go-flags` in `pkg/cli/root.go`, scope-name guards in `validateScopeName`/`requireScopeName`, config loading once in `Run()` and threaded through `Context.Config`.
- Existing validation style: `validateSigningKey` and `validateSuiteHeaders` in `pkg/config/config.go` are the templates to follow for `validateDistroName`.
- Existing test style: each `pkg/cli/*.go` has a sibling `*_test.go`; `pkg/config/config_test.go` uses table-driven cases.
- Removed code: `unionWithDiskDistros` (`pkg/cli/verify.go:208`) and its helpers go away — no replacement, scope source becomes `cfg.Distros` directly.

## Development Approach

- **Testing approach**: Regular (code first, then tests in the same task; matches `pkg/cli/*.go` + `*_test.go` sibling pattern).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
  - Tests are not optional — they are a required part of the checklist.
  - Write unit tests for new functions; update tests for modified functions.
  - Tests cover both success and error scenarios.
- **CRITICAL: all tests must pass before starting next task.**
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run `make test` after each task.

## Testing Strategy

- **Unit tests**: required for every task. Style: table-driven where the
  function-under-test has multiple input shapes (e.g. `validateDistroName`,
  `requireConfiguredDistro`). Use `t.TempDir()` for any file-creation tests.
- **Integration tests**: `pkg/cli/integration_test.go` and
  `integration_realpkg_test.go` already cover end-to-end repo creation,
  publish, and verify. Their fixture configs need `distros:` added — these
  tests are the safety net that catches wiring regressions.
- **No e2e/UI tests**: repokit is a CLI. The integration tests are the
  highest-level coverage in the repo.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.

## Solution Overview

Two coordinated changes:

1. **Config schema** gains a `Distros []string` field. `Load` validates each
   entry against a positive-allowlist (`[a-zA-Z0-9-]+`), rejects duplicates,
   and cross-checks that every `suites:` key's distro half is present in
   `distros:`. Empty/absent is permitted by `Load` itself — subcommands
   enforce non-membership.

2. **Subcommand enforcement.** A new helper `requireConfiguredDistro(cfg,
   distro)` in `pkg/cli/root.go` is wired into `add`, `delete`, `publish`,
   and `verify`. The publish-all-suites path filters its DB-resolved scope
   through `cfg.Distros` as defense-in-depth. The verify-all-distros path
   replaces `unionWithDiskDistros` with `cfg.Distros` directly — this is
   the bug fix.

`init`, `info`, `list`, `search` are unchanged. `init` updates its seed
config template to include a `distros:` block with `debian`/`ubuntu` examples
plus a header comment telling the operator to edit it.

**Hard cutover.** No transitional fallback. Existing configs without
`distros:` fail at command time with a clear error pointing the operator at
the config file. No prod deployments, so this is fine.

## Technical Details

### `pkg/config/config.go`

Add field:

```go
type Config struct {
    SigningKey string                 `yaml:"signing_key"`
    PublicDir  string                 `yaml:"public_dir"`
    Distros    []string               `yaml:"distros"`
    Suites     map[string]SuiteConfig `yaml:"suites"`
}
```

New helper, intentionally stricter than `validateScopeName`:

```go
func validateDistroName(name string) error {
    if name == "" {
        return errors.New("config: distros entry must not be empty")
    }
    for i := 0; i < len(name); i++ {
        c := name[i]
        ok := (c >= 'a' && c <= 'z') ||
              (c >= 'A' && c <= 'Z') ||
              (c >= '0' && c <= '9') ||
              c == '-'
        if !ok {
            return fmt.Errorf("config: distros entry %q: only [a-zA-Z0-9-] allowed", name)
        }
    }
    return nil
}
```

In `Load`, after the existing `Suites` loop, add:
- Iterate `c.Distros`, call `validateDistroName`, track a `seen map[string]struct{}` for duplicate detection.
- After validation, iterate `c.Suites` keys, call `splitSuiteKey` to extract the distro half, ensure it's in the validated set. Error message names the offending suite key.

### `pkg/cli/root.go`

Add helper:

```go
func requireConfiguredDistro(cfg *config.Config, distro string) error {
    if distro == "" {
        return errors.New("--distro is required")
    }
    for _, d := range cfg.Distros {
        if d == distro {
            return nil
        }
    }
    return fmt.Errorf("--distro %q is not in config.distros (configured: %v)", distro, cfg.Distros)
}
```

### `pkg/cli/add.go`, `delete.go`

Call `requireConfiguredDistro(c.Config, cmd.Distro)` after the existing
`requireScopeName(fieldDistro, cmd.Distro)`.

### `pkg/cli/publish.go`

- Scoped path (`--distro` set): call `requireConfiguredDistro` after the existing scope-name check.
- All-suites path involves two disk/DB sources unioned by `allScopes` (`pkg/cli/publish.go:436`):
  - `dbScopes` queries the packages table. Behavior unchanged — `add` already gates rows on `cfg.Distros`, so DB rows for an unconfigured distro only exist if config was edited to remove an in-use distro (operator error).
  - `filesystemScopes` (`pkg/cli/publish.go:511`) walks `<publicDir>/*/dists/` to find existing live trees — this is the second disk-walker, parallel to `unionWithDiskDistros`. Without a fix it would pick up `raspbian/dists/`, `test/dists/`, etc., and a no-arg `publish` would atomically replace those trees. **Change**: `filesystemScopes` takes `cfg.Distros` and iterates that list instead of reading top-level dirs. Drops the top-level `os.ReadDir(publicDir)`, the `validateScopeName` gate (entries from config are already validated by `validateDistroName`), and the disk-discovery semantics.
  - **Belt-and-braces filter in `allScopes`**: after unioning `dbScopes` and `filesystemScopes`, drop any scope whose distro is not in `cfg.Distros`. With `filesystemScopes` already constrained this is mostly redundant, but it catches the operator-error case where a config edit removed a distro that still has DB rows. Log a warning to `c.Stderr` when filtering drops a scope (helps operators notice the inconsistency).

### `pkg/cli/verify.go`

- Scoped path (`--distro` set): call `requireConfiguredDistro` after the existing scope-name check.
- Unscoped path: replace the `enumerate` callback + `unionWithDiskDistros` with iteration over `cfg.Distros`.
- **Delete** `unionWithDiskDistros` from `pkg/cli/verify.go`.
- **Simplify** `scanOrphansForScope`: remove the `enumerate func(context.Context) ([]string, error)` parameter. The unscoped branch just iterates `cfg.Distros` and calls `scanOrphans` per entry.
- **Remove the dead closure** in `verifyAndGC` (`pkg/cli/verify.go:151-153`) that wraps `r.EnumerateDistrosConn`. Once the parameter is gone, `verifyAndGC` no longer needs the `enumerate` argument it currently constructs.

### `pkg/repo/queries.go` (consequence of verify.go cleanup)

After the verify changes, `EnumerateDistros` (`pkg/repo/queries.go:177`) and
`EnumerateDistrosConn` (`pkg/repo/queries.go:186`) have no production
callers — only `pkg/repo/queries_test.go` exercises them. **Decision**:
delete both functions and their tests. They exist purely to feed the
now-removed `enumerate` callback in verify; keeping them as public API
without a caller is dead code. If a future feature needs the same query,
re-add it then.

### `pkg/cli/init.go`

Update the seed config template written by `init`:

```yaml
# Managed distros. Every --distro argument to add/delete/publish/verify
# must appear in this list. Edit before first use.
distros:
  - debian
  - ubuntu

signing_key: REPLACE_ME
public_dir: REPLACE_ME

suites:
  # debian/bookworm:
  #   origin: Example
  #   ...
```

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, doc updates listed below.
- **Post-Completion** (no checkboxes): operator-facing migration notes for any
  in-flight test or dev environment.

## Implementation Steps

### Task 1: Add `Distros` field and `validateDistroName` to config

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [x] add `Distros []string` field to `Config` struct in `pkg/config/config.go` with `yaml:"distros"` tag
- [x] add `validateDistroName(name string) error` helper using positive allowlist `[a-zA-Z0-9-]`, reject empty
- [x] in `Load`, after the existing `Suites` validation loop, iterate `c.Distros`: call `validateDistroName`, track seen names, error on duplicate
- [x] in `Load`, after distro validation, iterate `c.Suites` keys and reject any whose distro half (from `splitSuiteKey`) is not in the validated distro set
- [x] add table-driven tests for `validateDistroName` (accept: `debian`, `ubuntu-22`, `epel8`, `a`; reject: empty, `de bian`, `debian/sub`, `..`, leading `-`, `débian`, control chars)
- [x] add test: `Load` rejects duplicate `distros:` entries
- [x] add test: `Load` rejects `suites:` key whose distro half is not in `distros:`
- [x] add test: `Load` accepts a config with empty `distros:` (Load does not enforce non-emptiness; subcommands do)
- [x] run `make test ./pkg/config/...` — must pass before next task

### Task 2: Add `requireConfiguredDistro` helper

**Files:**
- Modify: `pkg/cli/root.go`
- Modify: `pkg/cli/root_test.go`

- [x] add `requireConfiguredDistro(cfg *config.Config, distro string) error` in `pkg/cli/root.go`
- [x] error message includes the offending value and the configured list so an operator can self-diagnose
- [x] add table-driven test: empty distro rejected, configured distro accepted, unconfigured distro rejected (error mentions the value and the list)
- [x] add test: nil-config / empty-distros case returns the unconfigured-distro error (rather than panicking)
- [x] run `make test ./pkg/cli/...` — must pass before next task

### Task 3: Wire `requireConfiguredDistro` into `add` and `delete`

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/delete.go`
- Modify: `pkg/cli/add_test.go`
- Modify: `pkg/cli/delete_test.go`

- [x] in `add`, call `requireConfiguredDistro(c.Config, cmd.Distro)` immediately after the existing `requireScopeName(fieldDistro, cmd.Distro)` call
- [x] in `delete`, same pattern
- [x] add `add_test.go` case: unconfigured `--distro` returns ExitError, no pool file written, no DB row inserted
- [x] add `delete_test.go` case: unconfigured `--distro` returns ExitError, no DB rows touched
- [x] update any existing `add`/`delete` tests whose fixture config lacks `distros:` — add the field listing the distros the test uses (handled centrally in `initRepoForTest` via a new `appendTestDistros` helper, also wired into the integration-test `runCmd("init")` callsites so the package-level test run keeps passing)
- [x] run `make test ./pkg/cli/...` — must pass before next task

### Task 4: Wire `requireConfiguredDistro` into `publish` and constrain disk discovery

**Files:**
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/publish_test.go`

- [x] in `publish`, when `cmd.Distro != ""`, call `requireConfiguredDistro` after the existing scope-name check
- [x] change `filesystemScopes(publicDir)` signature to `filesystemScopes(publicDir string, distros []string)`; iterate `distros` instead of reading top-level `<publicDir>` entries; for each, stat `<publicDir>/<distro>/dists/` and walk its suites as before (drop the `validateScopeName(fieldDistro, ...)` gate — entries from `cfg.Distros` are pre-validated)
- [x] thread `cfg.Distros` into `allScopes` so it can pass them to `filesystemScopes` (the simplest path: change `allScopes(ctx, publicDir, r)` → `allScopes(ctx, publicDir, distros, r)` and update its single caller in `resolveScopes`)
- [x] in `allScopes`, after the union, drop any scope whose distro is not in `cfg.Distros` (belt-and-braces against legacy DB rows); print a warning to `c.Stderr` per dropped scope so operators notice the inconsistency
- [x] add `publish_test.go` case: unconfigured `--distro` returns ExitError, no `<suite>.new` staging dir is created
- [x] add `publish_test.go` case: a `<publicDir>/raspbian/dists/bullseye/Release` exists on disk with `raspbian` **not** in `cfg.Distros`; no-arg `publish` does **not** touch the raspbian tree (the Release file is unchanged afterwards)
- [x] add `publish_test.go` case: a DB row exists for an unconfigured distro; no-arg `publish` skips it and writes a warning line to stderr
- [x] update fixture configs in existing `publish_test.go` cases to include `distros:` (handled via `initRepoForTest`; `TestPublishRequiresSigningKeyWithoutNoSign` rewrites the seed to drop signing_key but keeps a distros: block)
- [x] run `make test ./pkg/cli/...` — must pass before next task

### Task 5: Replace `unionWithDiskDistros` in `verify` with `cfg.Distros`, delete dead `repo.EnumerateDistros`

**Files:**
- Modify: `pkg/cli/verify.go`
- Modify: `pkg/cli/verify_test.go`
- Modify: `pkg/repo/queries.go`
- Modify: `pkg/repo/queries_test.go`

- [x] in `verify`, when `cmd.Distro != ""`, call `requireConfiguredDistro` after the existing scope-name check
- [x] remove the `enumerate func(context.Context) ([]string, error)` parameter from `scanOrphansForScope`; replace the unscoped branch with iteration over `cfg.Distros` and per-distro `scanOrphans` calls
- [x] remove the dead closure at `pkg/cli/verify.go:151-153` in `verifyAndGC` (the one that wraps `r.EnumerateDistrosConn`); update both `verifyAndGC` and `verifyOnly` callsites of `scanOrphansForScope` to match the new signature
- [x] **delete** `unionWithDiskDistros` from `pkg/cli/verify.go` (and any helpers with no remaining caller — `poolDirName` stays because `scopePoolDir` uses it)
- [x] verify `EnumerateDistros` / `EnumerateDistrosConn` have no other callers: `grep -rn EnumerateDistros /Users/alk/Development/repokit/pkg/` — confirmed only `pkg/repo/queries_test.go` references them after the verify changes
- [x] **delete** `EnumerateDistros` and `EnumerateDistrosConn` from `pkg/repo/queries.go`; delete their tests from `pkg/repo/queries_test.go`
- [x] add `verify_test.go` regression test (the bug being fixed): place `<publicDir>/raspbian/pool/foo.deb` on disk with `raspbian` **not** in `cfg.Distros`; run `verify --gc` (unscoped); assert the file is **not** deleted and `raspbian` is not reported as orphan
- [x] add `verify_test.go` case: unconfigured `--distro` returns ExitError without walking any pool (assert by leaving a known-orphan file in the configured-distro tree and verifying it is also untouched, since the run aborted before the walk)
- [x] delete or update existing tests that exercised the disk-discovery branch of `unionWithDiskDistros` (kept `TestVerifyUnscopedFindsOrphansInDBEmptyDistro`, which still exercises the no-DB-rows path via a configured distro; comment updated to reflect the new semantics)
- [x] update fixture configs in existing `verify_test.go` cases to include `distros:` (already handled centrally via `initRepoForTest` → `appendTestDistros` from Task 3)
- [x] run `make test ./pkg/cli/... ./pkg/repo/...` — must pass before next task

### Task 6: Update `init` seed config template

**Files:**
- Modify: `pkg/cli/init.go`
- Modify: `pkg/cli/init_test.go`

- [x] update the seed YAML written by `init` to include a `distros:` block at the top with `debian` and `ubuntu` example entries, plus a header comment instructing the operator to edit before first use
- [x] preserve the existing `signing_key: REPLACE_ME` and `public_dir: REPLACE_ME` placeholders
- [x] add test: the seed config written by `init` parses cleanly via `config.Load` (covered by existing `TestInitConfigTemplateRoundTrip`, which now exercises distros validation as a side effect)
- [x] add test: parsed seed config has a non-empty `Distros` slice (new `TestConfigTemplateHasDistros`)
- [x] add test: parsed seed config's `Distros` entries all pass `validateDistroName` (asserted via `[a-zA-Z0-9-]+` regex in `TestConfigTemplateHasDistros`)
- [x] run `make test ./pkg/cli/...` — must pass before next task (also updated `appendTestDistros` to strip pre-existing distros: blocks so the new template doesn't clash with test fixtures)

### Task 7: Update integration test fixtures

**Files:**
- Modify: `pkg/cli/integration_test.go`
- Modify: `pkg/cli/integration_realpkg_test.go`

- [x] grep for `signing_key:` in both files to find fixture configs; add a `distros:` block listing the distros each fixture exercises (no inline fixture configs — both integration tests use `initRepoForTest` + `appendTestDistros` from Task 3, so the `distros:` block is already wired in centrally)
- [x] no behavioral changes — these tests must continue to pass end-to-end (`init` → `add` → `publish` → `verify`)
- [x] run `make test ./pkg/cli/...` — full integration suite must pass before next task

### Task 8: Verify acceptance criteria

- [x] verify all Overview requirements are implemented: `distros:` field exists, all write commands gated, `unionWithDiskDistros` removed, init template updated
- [x] verify the regression case manually: built the binary, created a temp public dir with bogus `raspbian/pool/test.deb`, ran `repokit verify --gc` with config listing only `debian`/`ubuntu`, file remained intact (`verify: ok`, file present afterwards)
- [x] run `make test` — full test suite passes
- [x] run `make race` — race sweep passes
- [x] run `make lint` — no new lint findings (fixed two preexisting findings introduced by this plan: `for i := 0; i < len(name)` → `for i := range len(name)` in `validateDistroName`; `len(rewritten) > 0` → `rewritten != ""` in `appendTestDistros`)

### Task 9: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [x] in `CLAUDE.md`, under Architecture / `Context.Config`, add a sentence: `Distros` is the authoritative list of managed distros; subcommands reject `--distro` values not in this list.
- [x] in `CLAUDE.md` Gotchas, remove any wording implying on-disk distro discovery; add: "Managed distros are explicit in `config.yaml`. The public dir may contain unrelated top-level directories — repokit never walks them. Adding a new distro requires a config edit before `add` will accept it."
- [x] in `README.md`, add a one- or two-paragraph note in the config section describing the `distros:` field with a YAML example (also brought `docs/usage.md` in line: stripped the disk-discovery wording from the `publish` section and added `distros:` to the config-schema table and example)
- [x] move this plan to `docs/plans/completed/`: `mkdir -p docs/plans/completed && git mv docs/plans/20260512-explicit-distros.md docs/plans/completed/`

## Post-Completion

*Informational only — no checkboxes, no automated action.*

**Manual verification:**
- On any existing dev environment where repokit was previously used, the
  config file at `~/.repokit/config.yaml` will need a `distros:` field added
  by hand. Commands will fail with a clear error pointing at this until the
  edit is made.

**Follow-up task (separate plan):**
- Allow `/` in distro names to support customer-tree layouts
  (`<publicDir>/customer/cust1/pool/...`). Touches `validateScopeName`,
  `splitSuiteKey` in `pkg/config/config.go`, `pool.DiskPath`,
  `scopePoolDir`, and the suite-key syntax decision (greedy match against
  `cfg.Distros`? nested YAML map? separator change?). Best opened as a new
  plan when the customer-tree use case becomes concrete.
