# Split state and public dirs

## Overview

Today `REPOKIT_ROOT` holds both private state (`config.yaml`, `repo.db`) and the
public-facing apt tree (`<distro>/pool/`, `<distro>/dists/`). The original
deployment model rsynced individual `<distro>/` directories to a CDN, so the
private files at the root were never exposed. The model has shifted: signing
now happens on the server (forwarded gpg-agent), the DB lives there too, and
the same box is the public webserver. A docroot pointed at `REPOKIT_ROOT`
would leak `config.yaml` and `repo.db`.

This plan separates the two concerns:

- `REPOKIT_ROOT` becomes the **state dir** — only `config.yaml` and `repo.db`.
  Defaults to `~/.repokit/`.
- A new `public_dir` (config.yaml field, with `--public` flag and
  `REPOKIT_PUBLIC` env override) is the **public dir** — holds
  `<distro>/{pool,dists}/`. No default; operator must set it.

Webserver docroot points at `public_dir` and exposes the apt tree as-is.

This is a **breaking change**. The project was renamed from `debrepo` recently
and is not deployed in production, so no migration code is shipped.

## Context (from discovery)

Files that need changes:

- `pkg/config/config.go` — add `PublicDir` field + validation
- `pkg/cli/root.go` — `Options.Public`, `Context.PublicDir`,
  `resolvePublicDir()`, dispatch wiring
- `pkg/cli/init.go` — default `~/.repokit/` resolution, new config template
- `pkg/cli/add.go` — pool I/O reads `ctx.PublicDir` not `ctx.Root`
- `pkg/cli/delete.go` — `pool.Delete` reads `ctx.PublicDir`
- `pkg/cli/publish.go` — `distsParent`, `filesystemScopes`, `pool.MkdirAllSync`
  read `ctx.PublicDir`; `loadPublishConfig` still reads `ctx.Root`
- `pkg/cli/verify.go` — pool walks read `ctx.PublicDir`; the `poolDirName`
  constant comment becomes incorrect (pool no longer rooted at state root)
- `pkg/cli/integration_test.go`, `pkg/cli/integration_realpkg_test.go` — split
  single tmp dir into `tmpState` + `tmpPublic`
- Per-command unit tests (`add_test.go`, `delete_test.go`, `publish_test.go`,
  `verify_test.go`) — set up split dirs in each fixture
- `pkg/config/config_test.go` — new validation cases
- `pkg/cli/init_test.go` — new default-`~/.repokit/` cases via injected home func
- `README.md`, `CLAUDE.md`, `docs/usage.md` — document new layout and env vars

Decisions already made (do not re-litigate):

1. Two independent paths via `--public`/`REPOKIT_PUBLIC`/`config.yaml`.
2. State default: `~/.repokit/` (single dotdir, no XDG split).
3. Public resolution precedence: flag > env > config > error.
4. Public validation: absolute, exists, not equal to or nested inside state,
   no control chars, no `REPLACE_ME` placeholder. Do not auto-create.
5. `init` default creates `~/.repokit/`; positional path arg still works; no
   `init --public` flag — operator edits `config.yaml` like with `signing_key`.
6. DB schema unchanged — pool paths stay relative.
7. Hard break, no migration code, no auto-detect, no `repokit migrate`.
8. Out of scope: multiple public dirs, auto-creating public, cross-distro
   pool symlinking, a `repokit env` introspection subcommand.

## Development Approach

- **testing approach**: Regular (code first, then tests) — each task writes
  the implementation, then adds/updates tests within the same task before
  moving on.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes
- **CRITICAL: all tests must pass before starting next task**
- run `make test` after each task

## Testing Strategy

- **unit tests**: required per task. Table-driven where the project already
  uses that style. New tests live next to existing `*_test.go`.
- **integration tests**: `integration_test.go` and `integration_realpkg_test.go`
  are updated in their own task once all per-command wiring is done.
- no UI / e2e tests in this project.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document blockers with ⚠️ prefix
- update plan if scope changes during implementation

## Solution Overview

Resolution flow added to `pkg/cli/root.go`:

```
Run(argv):
  parse flags into Options
  resolveRepoRoot(opts.Repo, cmd) → Context.Root        (existing)
  if needsPublic(cmd):                                  (new)
      resolvePublicDir(opts.Public, cfg, root) → Context.PublicDir
  dispatch to subcommand
```

`needsPublic` returns `true` for `add`, `delete`, `publish`, `verify`; `false`
for `init`, `info`, `list`, `search`. Resolution loads `config.yaml` once
through the same path `loadPublishConfig` already uses.

Existing call sites that today pass `c.Root` to `pool.*` / `os.Stat` for the
public tree are mechanically swapped to `c.PublicDir`. The DB still lives at
`filepath.Join(c.Root, dbFilename)`.

`init` gains a `defaultStateDir func() (string, error)` indirection so tests
can inject a fake home-dir resolver and assert on the resulting path without
touching the real `$HOME`.

## Technical Details

**Config additions** (`pkg/config/config.go`):

```go
type Config struct {
    SigningKey string                 `yaml:"signing_key"`
    PublicDir  string                 `yaml:"public_dir"`   // NEW
    Suites     map[string]SuiteConfig `yaml:"suites"`
}
```

`Load` validates `PublicDir` only if present (newline/control-char rejection,
mirrors `validateSigningKey`). Absoluteness, existence, and the equal/nested
checks happen in `resolvePublicDir` because they need the resolved state root
for comparison.

**CLI options** (`pkg/cli/root.go`):

```go
type Options struct {
    Repo   string `long:"repo"   short:"r" env:"REPOKIT_ROOT"   ...`
    Public string `long:"public" short:"p" env:"REPOKIT_PUBLIC" ...`
    Quiet  bool   ...
    // ...subcommands
}

type Context struct {
    Root      string
    PublicDir string  // NEW; empty for init/info/list/search
    Stdout    io.Writer
    Stderr    io.Writer
    Quiet     bool
}
```

`resolvePublicDir(flag, cfg, root) (string, error)`:
- pick first non-empty of: `flag`, `os.Getenv("REPOKIT_PUBLIC")` (already
  bound by go-flags into `flag` if no `-p`/`--public` given — so this is
  effectively flag-or-env > config), `cfg.PublicDir`
- treat literal `"REPLACE_ME"` as missing with a friendly message
- error if empty
- error if not absolute
- error if `os.Stat` fails or target is not a directory
- error if `path == root` or `path` is inside `root` (and vice versa: `root`
  must not be inside `path`)
- reject control chars / NUL

**State default** (uniform across all commands):

`~/.repokit/` is the default for **every** subcommand when neither
`--repo` nor `$REPOKIT_ROOT` is set. The cwd-fallback that
`resolveRepoRoot` does today is **removed** so resolution is
predictable regardless of where the operator runs the binary from.

New resolution chain for all commands:

```
--repo flag > $REPOKIT_ROOT env > ~/.repokit/ (default)
```

This applies to `init` (where the default is the dir to create) and to
every other command (where the default is the dir whose `repo.db` gets
opened — and which still errors with `ErrRepoNotFound` when the file is
missing).

Implementation: `resolveRepoRoot` is simplified — flag > env >
`defaultStateDir()`. The cwd branch is deleted entirely. `init`'s
positional argument still wins over everything else. `defaultStateDir =
func() (string, error) { home, err := os.UserHomeDir(); ...; return
filepath.Join(home, ".repokit"), nil }` is a package-level var so tests
can swap it.

**Shared config loader** (`pkg/cli/`):

`publish.go` already has `loadPublishConfig(root)` which gracefully handles a
missing `config.yaml` (returns an empty `*config.Config`). `resolvePublicDir`
needs the same behavior, so the loader is extracted to a shared location
(rename to `loadConfig` in `root.go`) and called once from `Run()` before
both `resolvePublicDir` and `publish.go`. Avoids two near-identical loaders
diverging silently.

**Init template** (config.yaml content, `init.go` constant):

```yaml
# repokit configuration

# gpg key id or fingerprint that signs Release / InRelease.
signing_key: "REPLACE_ME"

# absolute path to the public-facing tree (pool/ + dists/).
# point your webserver's docroot at this directory.
public_dir: "REPLACE_ME"

suites:
  # ...same as today
```

**Per-command wiring**: replace `c.Root` with `c.PublicDir` at every pool /
dists I/O site. The DB-opening lines (`repo.Open(ctx, filepath.Join(c.Root,
dbFilename))`) stay on `c.Root`. `loadPublishConfig(c.Root)` stays on
`c.Root` because `config.yaml` lives in the state dir.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, test, and doc changes
  inside this repo.
- **Post-Completion** (no checkboxes): operator-side actions like updating
  systemd units / cron entries / Ansible roles that referenced the old
  `REPOKIT_ROOT` semantics. Outside this repo.

## Implementation Steps

### Task 1: Add `public_dir` field and validation to config

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [x] add `PublicDir string \`yaml:"public_dir"\`` field to `Config`
- [x] inline a control-char / newline / NUL rejection in `Load` for
      `c.PublicDir` (only when non-empty). Don't introduce a "Header" helper
      — `public_dir` is a path string, not a Release header field, so the
      `signing_key`/suite-fields naming convention doesn't fit. A few inline
      `strings.ContainsAny` lines is plenty.
- [x] absolute / exists / nesting checks live in `resolvePublicDir`, not
      here — they need the resolved state root for comparison
- [x] write tests: parses valid `public_dir`; empty `public_dir` is accepted
      at load time (resolution will reject); rejects newline; rejects NUL;
      rejects control chars
- [x] run `go test ./pkg/config/... -race -count=1` — must pass

### Task 2: Default state dir to `~/.repokit/` for all commands

**Files:**
- Modify: `pkg/cli/init.go`
- Modify: `pkg/cli/root.go`
- Modify: `pkg/cli/init_test.go`
- Modify: `pkg/cli/root_test.go`

- [x] introduce package-level `defaultStateDir = func() (string, error) {
      home, err := os.UserHomeDir(); ... return filepath.Join(home,
      ".repokit"), nil }` in `root.go` so tests can swap it
- [x] simplify `resolveRepoRoot`: chain becomes `flagRepo` > `$REPOKIT_ROOT`
      > `defaultStateDir()`. **Delete the cwd fallback entirely.** Keep the
      `repo.db` stat gate for non-init/non-info; keep the init/info
      short-circuit so a fresh `~/.repokit/` (which doesn't exist yet) is
      not stat'd on `init`.
- [x] update `InitCmd.resolveRoot` chain: positional arg > `ctxRoot`
      (which is now always non-empty thanks to the default in
      `resolveRepoRoot`). The init-side default helper is no longer
      needed because `ctxRoot` already carries `defaultStateDir()`.
- [x] update `configTemplate` to include `public_dir: "REPLACE_ME"` with a
      comment matching the `signing_key` style
- [x] write tests: `repokit init` (no args, no flag, no env) creates
      `<fakeHome>/.repokit/` with `config.yaml` and `repo.db`;
      `repokit init /tmp/x` still wins (positional > default);
      `--repo /custom` overrides the default for both init and non-init
- [x] write test: `resolveRepoRoot("", "add")` with `defaultStateDir`
      pointing at a populated tempdir resolves without error and hits the
      `repo.db` check successfully
- [x] write test: `resolveRepoRoot("", "add")` with `defaultStateDir`
      pointing at an empty tempdir returns `ErrRepoNotFound` (proves the
      default is applied uniformly, not silently bypassed)
- [x] write test: `resolveRepoRoot("", "init")` with `defaultStateDir`
      pointing at an empty tempdir returns the path with no error (init
      skips the repo.db check)
- [x] write test: cwd is no longer consulted — running with `os.Chdir`
      into a directory that contains a `repo.db` does not affect
      resolution
- [x] write test: template constant contains `public_dir: "REPLACE_ME"`
- [x] run `go test ./pkg/cli/... -race -run 'Init|ResolveRepoRoot' -count=1` —
      must pass

### Task 3: Add `--public` flag, `resolvePublicDir`, and `Context.PublicDir`

**Files:**
- Modify: `pkg/cli/root.go`
- Modify: `pkg/cli/publish.go` (extract `loadPublishConfig` to shared helper)
- Modify: `pkg/cli/root_test.go`

- [x] extract `loadPublishConfig` from `publish.go` into a shared helper in
      `root.go` (rename to `loadConfig`); update `PublishCmd.run` to call
      the shared version. Keep the same "missing file → empty Config"
      semantics so a freshly init'd repo whose operator hasn't finished
      editing `config.yaml` still gets a useful error message from
      `resolvePublicDir`, not a parse error.
- [x] add `Public string \`long:"public" short:"p" env:"REPOKIT_PUBLIC" ...\``
      to `Options`. Note: go-flags merges the env var into `opts.Public`
      automatically; do **not** call `os.Getenv("REPOKIT_PUBLIC")` again in
      `resolvePublicDir`.
- [x] add `PublicDir string` to `Context`
- [x] add `needsPublic(cmd string) bool` returning true for `add`, `delete`,
      `publish`, `verify`; false for `init`, `info`, `list`, `search`
- [x] add `resolvePublicDir(flagOrEnv string, cfg *config.Config, root
      string) (string, error)`. The cfg arg is loaded once in `Run()` via
      the shared `loadConfig` and threaded through. Logic: pick
      `flagOrEnv` (flag-or-env, already merged by go-flags) > `cfg.PublicDir`;
      reject `"REPLACE_ME"` with `public_dir is not set: edit
      <root>/config.yaml or pass --public / REPOKIT_PUBLIC`; reject relative
      paths; require directory existence with `public_dir <path>: directory
      does not exist (create it manually before running)`; reject
      `path == root`, `path` inside `root`, and `root` inside `path` with
      `public_dir <path> must not equal or be nested inside REPOKIT_ROOT
      <root>`
- [x] in `Run()`, after `resolveRepoRoot` succeeds and before dispatch:
      load config via `loadConfig(root)` once (skip for `init` since
      config may not exist yet); when `needsPublic(cmd)`, call
      `resolvePublicDir(opts.Public, cfg, root)` and populate
      `ctx.PublicDir`; return `ExitError` with the wrapped message on
      failure. The same `cfg` is reusable inside `PublishCmd.run` — pass
      via `Context` to avoid loading twice.
- [x] write tests: precedence (flag/env wins over config); each validation
      rule errors with the specified message; `needsPublic` returns the
      correct boolean for each subcommand name; `init`/`list` do not invoke
      `resolvePublicDir`; `loadConfig` returns an empty Config on missing
      file
- [x] run `go test ./pkg/cli/... -race -run 'Run|Resolve|Config' -count=1` —
      must pass

### Task 4: Wire `add` to use `ctx.PublicDir`

**Files:**
- Modify: `pkg/cli/add.go`
- Modify: `pkg/cli/add_test.go`

- [x] swap every `c.Root` passed to `pool.*`, `pool.RollbackReplace`,
      `pool.FinalizeReplace`, `pool.Delete`, `writePool`, and the
      `cmd.reconcileBackup` body (`filepath.Join(c.Root, ...)` for
      `dest+".bak"` lookup) to `c.PublicDir`
- [x] keep `repo.Open(ctx, filepath.Join(c.Root, dbFilename))` on `c.Root`
- [x] update `add_test.go` fixtures to set up `tmpState` (state root) and
      `tmpPublic` (public dir) and populate `Context{Root: tmpState,
      PublicDir: tmpPublic}` directly, bypassing `resolvePublicDir` for unit
      tests
- [x] write tests: pool file lands under `<tmpPublic>/<distro>/pool/...`
      (not under `tmpState`); `--replace` rollback restores the prior bytes
      under `tmpPublic`; DB row still lives in `<tmpState>/repo.db`
- [x] run `go test ./pkg/cli/... -race -run 'Add' -count=1` — must pass

### Task 5: Wire `delete` to use `ctx.PublicDir`

**Files:**
- Modify: `pkg/cli/delete.go`
- Modify: `pkg/cli/delete_test.go`

- [x] swap `pool.Delete(c.Root, ...)` to `pool.Delete(c.PublicDir, ...)`
- [x] keep `repo.Open(ctx, filepath.Join(c.Root, dbFilename))` on `c.Root`
- [x] update `delete_test.go` fixtures to use split dirs
- [x] write tests: pool unlink targets the public dir; partial pool failure
      mid-loop still keeps the DB committed (existing test, just fixture
      update); `--dry-run` does not touch either dir
- [x] run `go test ./pkg/cli/... -race -run 'Delete' -count=1` — must pass

### Task 6: Wire `publish` to use `ctx.PublicDir`

**Files:**
- Modify: `pkg/cli/publish.go`
- Modify: `pkg/cli/publish_test.go`

- [x] in `publishScopeLocked`: `distsParent := filepath.Join(c.PublicDir,
      distro, distsDir)` (was `c.Root`)
- [x] in `filesystemScopes(root)`: rename argument to `publicDir` and have
      the caller pass `c.PublicDir`. Update the doc comment at
      `publish.go:520-530` — `<root>` becomes `<publicDir>`; drop the
      `repo.db, config.yaml, pubkey.asc` parenthetical (those no longer
      live in the public dir) but keep the `validateScopeName` rationale
      for skipping arbitrary user-created top-level dirs.
- [x] in `allScopes(ctx, root, r)`: rename to take `publicDir`, pass through
      to `filesystemScopes`
- [x] replace the local `loadPublishConfig` call with the shared
      `loadConfig(c.Root)` introduced in Task 3 — config still lives in
      the state dir, only the loader location moved
- [x] update `publish_test.go` fixtures (and any helper that builds a fake
      live tree) to use `tmpPublic`
- [x] write tests: published `dists/` lands under `<tmpPublic>/<distro>/...`;
      `<suite>.new`/`<suite>.old` staging is under `tmpPublic`; recovery
      from a stranded `.old` still works against `tmpPublic`;
      `filesystemScopes` walks `tmpPublic` not `tmpState`
- [x] run `go test ./pkg/cli/... -race -run 'Publish' -count=1` — must pass

### Task 7: Wire `verify` to use `ctx.PublicDir`

**Files:**
- Modify: `pkg/cli/verify.go`
- Modify: `pkg/cli/verify_test.go`

- [x] swap `verifyOnly`, `verifyAndGC`, `scanOrphansForScope`,
      `unionWithDiskDistros`, `scanOrphans`, `removeOrphans`, and `verifyOne`
      to take/use `c.PublicDir` instead of `c.Root`
- [x] update the `unionWithDiskDistros` doc comment — "top-level
      `<root>/<distro>` directory" becomes "`<publicDir>/<distro>`"; remove
      mentions of `repo.db, config.yaml, pubkey.asc` as files we skip at the
      top level (they no longer live there); the `validateScopeName` skip is
      still useful for arbitrary user dirs
- [x] revisit the `poolDirName` constant comment in `verify.go` (line 49–51:
      "no longer a top-level directory under `<root>`") — its meaning
      changes again: pool now lives under `<publicDir>/<distro>/pool/`
- [x] update `verify_test.go` fixtures to use split dirs
- [x] write tests: hash verification reads from `tmpPublic`; orphan walk
      visits `tmpPublic`; `--gc` removes orphans under `tmpPublic` only;
      `tmpState` (which contains `repo.db` and `config.yaml`) is never
      touched by any verify pass
- [x] run `go test ./pkg/cli/... -race -run 'Verify' -count=1` — must pass

### Task 8: Update integration tests to use split dirs

**Files:**
- Modify: `pkg/cli/integration_test.go`
- Modify: `pkg/cli/integration_realpkg_test.go`

- [x] replace the single `tmpDir := t.TempDir()` pattern with `tmpState :=
      t.TempDir()` + `tmpPublic := t.TempDir()`
- [x] each `Run` invocation passes `--repo=<tmpState>` and either
      `--public=<tmpPublic>` or sets `REPOKIT_PUBLIC` via `t.Setenv`
- [x] update post-`init` step to write `public_dir: <tmpPublic>` into the
      generated `config.yaml` (replace the `REPLACE_ME` placeholder) so
      subsequent commands resolve through the config path on at least one
      test, while another test exercises the flag/env paths
- [x] write/adjust assertions: pool files end up under `tmpPublic`,
      `dists/` lands under `tmpPublic`, `repo.db` and `config.yaml` stay
      under `tmpState`
- [x] add an explicit assertion: `<tmpState>` contains exactly
      `{config.yaml, repo.db}` (no `<distro>/` directory leaked there)
- [x] run `go test ./pkg/cli/... -race -run 'Integration' -count=1` — must pass
- [x] run full `make test` — must pass (also fixed list_test.go / search_test.go
      `seedTwoSuites` helper which was broken since Task 4)

### Task 9: Documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `docs/usage.md` (if it covers layout/flags)

- [x] README.md: update the "quick start" / layout section. State that
      `repokit init` now defaults to `~/.repokit/`; document the new
      `public_dir` config field, the `--public` flag, and `REPOKIT_PUBLIC`
      env var; add a "breaking change" note pointing to the v? release.
- [x] CLAUDE.md: update the "Architecture" and "Gotchas" sections — the
      flow-of-typical-write-command paragraph still says everything writes
      under `<repo root>`; replace with the split (state vs public);
      document `resolvePublicDir` precedence; note that `config.yaml`
      lives in `REPOKIT_ROOT` and `pool/`+`dists/` live in `REPOKIT_PUBLIC`
- [x] CLAUDE.md: update the existing "Repo-root resolution: `--repo` >
      `$REPOKIT_ROOT` > cwd" sentence to the new uniform chain: `--repo`
      > `$REPOKIT_ROOT` > `~/.repokit/` (default). cwd is no longer
      consulted. Document `--public` / `REPOKIT_PUBLIC` /
      `config.yaml: public_dir` separately.
- [x] docs/usage.md: walk through the new init+config flow end-to-end;
      include a sample config.yaml with both `signing_key` and `public_dir`
      filled in; show a webserver snippet (nginx or apache) pointing at
      `public_dir`
- [x] no test changes; verify `make build` still succeeds

### Task 10: Verify acceptance criteria
- [x] all decisions in the brainstorm are implemented (re-read Overview)
- [x] `repokit init` with no args creates `~/.repokit/` with the new template
      (covered by `init.go:29` template + `init_test.go` default-home tests)
- [x] `repokit add` / `delete` / `publish` / `verify` all error helpfully
      when `public_dir` is unset (covered by `root.go:254/272/282` in
      `resolvePublicDir`, exercised in `root_test.go` and integration tests)
- [x] running an `add` then `publish` then `verify` against split dirs
      produces a valid apt repo under the public dir, with `repo.db` /
      `config.yaml` not leaked there (covered by integration tests in
      Task 8: explicit state-dir-only-contains-{config.yaml, repo.db}
      assertion)
- [x] run `make lint` — pre-existing goconst pattern (per-command
      string literals in dispatch switch) gains a new instance for
      "verify"; matches the 7 already-accepted instances on main, no
      new categories introduced. testifylint warnings on the new
      tests fixed. fix-all would require introducing command-name
      constants across the file, which is out of scope.
- [x] run `make test` — all green (5.488s pkg/cli, all packages ok)
- [x] run `make race` — green (60s timeout, all packages ok)

### Task 11: Final — move plan
- [x] `mkdir -p docs/plans/completed`
- [x] `git mv docs/plans/20260509-split-state-and-public.md
      docs/plans/completed/`

## Post-Completion

*Operator-side actions (outside this repo):*

**Manual verification:**

- create a fresh `~/.repokit/`, fill in `signing_key` + `public_dir`, run a
  full `init → add → publish → verify` cycle against a real `.deb`
- point an nginx/apache vhost at `public_dir`, run `apt update` from a
  client against it, install a published package
- confirm `~/.repokit/` is not exposed by the webserver (no traversal,
  no symlink leak)

**External system updates:**

- update any systemd units, cron entries, Ansible roles, or shell aliases
  that reference `REPOKIT_ROOT` with the old combined-dir semantics; either
  set `REPOKIT_PUBLIC` alongside, or migrate to the `~/.repokit/` default
- update CI / release pipelines if they consume the layout (e.g. goreleaser
  asset paths) — none expected, but worth a sweep
