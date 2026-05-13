# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`repokit` is a minimal Debian apt repository manager. It maintains static
per-distro `<distro>/pool/` + `<distro>/dists/` trees backed by SQLite, with
explicit publish-and-sign and atomic dist swaps. Hashes are computed once at
`add` and persisted; `publish` only re-emits them.

Module path: `github.com/alkk/repokit`. Single binary at `cmd/repokit`.

## Common commands

| Command | Purpose |
|---|---|
| `make build` | builds `.bin/repokit` (and `.bin/repokit.<branch>`) with embedded `main.revision` |
| `make test` | `go test -race -coverprofile` over all packages, mocks stripped from coverage |
| `make race` | quick race sweep, 60s timeout |
| `make lint` | `golangci-lint run` |
| `make fmt` | `gofmt -s` + `goimports` (skips `vendor/` and `mocks/`) |
| `make version` | print the revision string the build would embed |

Run a single test: `go test ./pkg/cli -run TestList -count=1` (use `-race` to
match CI). Vendored deps live in `vendor/` — `go build`/`go test` use them
automatically.

## Architecture

Subcommand dispatch is centralized in `pkg/cli/root.go` via a single
`jessevdk/go-flags` parser. Each subcommand is a `*Cmd` struct on `Options`
with its flags as struct tags; `Run()` resolves the state dir + public dir,
builds a shared `Context`, and dispatches by `parser.Active.Name`. Adding a
subcommand means adding a field to `Options`, a case in the `switch`, and a
`<name>.go` file in `pkg/cli/`.

`Context` carries the resolved environment for the dispatched subcommand:

- `Context.Root` — the **state dir**: holds `repo.db` and `config.yaml`. Set
  for every subcommand.
- `Context.PublicDir` — the **public dir**: holds `<distro>/pool/` and
  `<distro>/dists/`. Set only for `add`, `delete`, `publish`, `verify`
  (`needsPublic` in `root.go`); empty for `init`, `info`, `list`, `search`.
- `Context.Config` — parsed `config.yaml` (or an empty `*config.Config` when
  the file is missing). Loaded and validated once in `Run()` for every command
  except `init` (creates the file) and `info` (never reads state) — see
  `needsConfig`. Subcommand handlers should read `c.Config` instead of
  re-calling `loadConfig(c.Root)`. `Config.Distros` is the authoritative list
  of managed distros; `add`, `delete`, `publish`, and `verify` reject
  `--distro` values not in this list via `requireConfiguredDistro` in
  `root.go`. Distro names may be plain (`debian`) or one-level namespaced
  (`customer/acme`, producing nested `<publicDir>/customer/acme/{pool,dists}/`);
  a plain-prefix conflict like `[customer, customer/acme]` is rejected at
  config load.

Flow of a typical write command (`add`, `delete`, `publish`):

1. `pkg/cli/<cmd>.go` parses flags, validates scope names with
   `validateScopeName` / `requireScopeName` (defends against path traversal
   into `dists/`/`pool/` and against Release-header injection; also reserves
   `.new`/`.old` suite suffixes for publish staging). `--distro` is the
   exception: it goes through `validateDistroScope` / `requireDistroScope`
   (in `root.go`), which delegate to `config.ValidateDistroName` so the
   one-level namespaced form (`customer/acme`) is accepted while the other
   scope fields keep `validateScopeName` (which still rejects `/`).
   `publish`'s `dbScopes` discovery path must use `validateDistroScope` too —
   `validateScopeName` there would silently drop every namespaced row.
2. `pkg/repo` opens `repo.db` from `Context.Root` (modernc.org/sqlite, pure
   Go), runs any pending migrations from `pkg/repo/migrate.go`, and exposes
   typed queries in `pkg/repo/queries.go`. All multi-step writes happen
   inside a single transaction.
3. `pkg/deb` parses `.deb` control metadata; `pkg/pool` writes pool files
   under `Context.PublicDir` (with `pool_unix.go` / `pool_windows.go` split
   for atomic rename semantics).
4. `publish` rebuilds `<publicDir>/<distro>/dists/<suite>.new/` via
   `pkg/index` (`packages.go` for `Packages{,.gz,.xz}`, `release.go` for
   `Release` / `InRelease` / `Release.gpg`), signs through `pkg/sign`
   (gpg-agent; never prompts), then atomically swaps `.new` → live → `.old`
   and cleans up. The `<suite>.new` staging dir is also cleaned on failure.

State-dir resolution: `--repo` > `$REPOKIT_ROOT` > `~/.repokit/` (default).
The cwd is **not** consulted. Every command except `init` and `info` requires
`repo.db` at the resolved state dir.

Public-dir resolution (`resolvePublicDir` in `root.go`): `--public` /
`$REPOKIT_PUBLIC` (merged by go-flags into `opts.Public`) > `public_dir` in
`config.yaml` (loaded once via `loadConfig` and threaded through
`Context.Config`). The resolver rejects empty / `REPLACE_ME`, relative paths,
non-existent or non-directory targets, control characters, and any path that
equals or is nested with the state root in either direction (a leak would put
`repo.db` under the webserver docroot — the whole motivation for the split).
The public dir is never auto-created — operators must `mkdir` it themselves.

Signing: `pkg/sign/gpg.go` shells out to `gpg` and relies entirely on
`gpg-agent` for the passphrase — `repokit` never prompts. `pkg/sign/nop.go`
backs `--no-sign`.

Tests: each `pkg/cli/*.go` has a sibling `*_test.go`. `integration_test.go`
and `integration_realpkg_test.go` (the latter consumes prebuilt fixtures from
`test-packages/`) exercise full end-to-end repo creation, publish, and
verify.

## Gotchas

- Goreleaser v2 deprecated `archives.builds` and `nfpms.builds`; use `ids:` instead.
- `go-flags` aliases: `command:"list" alias:"ls"` — `parser.Active.Name`
  still returns the canonical `"list"`, so the dispatch `switch` does not
  need a separate case for the alias.
- Suite names ending in `.new` / `.old` are rejected at flag-validation time
  because publish uses those siblings as staging dirs and would wipe them.
- `--replace` on `add` accepts only one `.deb` per invocation (rolling back
  an overwritten pool file is unsafe inside a multi-package batch).
- `Context.Root` and `Context.PublicDir` are not interchangeable. Pool / dists
  I/O must read `c.PublicDir`; `repo.Open` and `loadConfig` must read
  `c.Root`. Mixing them either leaks state into the public docroot or makes
  the webserver miss the data. When wiring a new subcommand, mirror the
  existing handlers and add it to `needsPublic` only if it actually touches
  pool/dists.
- `os.Getenv("REPOKIT_PUBLIC")` is **not** read directly — the env tag on
  `Options.Public` lets go-flags merge it into `opts.Public`. Calling
  `os.Getenv` again in `resolvePublicDir` would double-resolve and break the
  precedence rule that `--public` wins over the env var.
- The default state dir is `~/.repokit/` for **every** subcommand including
  `init`. Tests must override `defaultStateDir` (package-level var in
  `root.go`) rather than poke `$HOME`.
- Managed distros are explicit in `config.yaml`. The public dir may contain
  unrelated top-level directories — repokit never walks them. Adding a new
  distro requires a config edit before `add` will accept it.
- `config.suites` keys are `<distro>/<suite>` and `splitSuiteKey` cuts on the
  **last** `/`, so a namespaced distro's per-suite override key has three
  segments (`customer/acme/stable` → distro `customer/acme`, suite `stable`).
  The distro half is then re-validated by `ValidateDistroName`, which rejects
  a left half carrying more than one `/`.
