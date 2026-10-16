# repokit review profile

## What it is
- Single Go binary (`cmd/repokit`) managing static Debian apt repos: SQLite state (`repo.db`, `config.yaml`) in a private state dir, `<distro>/pool/` + `<distro>/dists/` in a separate public docroot.
- Operator-run CLI on a repo host (packages.netxms.org). No daemon, no network listener; `gpg` via gpg-agent for signing.
- Ships linux/darwin via goreleaser; must still compile for Windows (`pool_windows.go`).

## What a real failure looks like
- apt clients break: a published index pointing at a missing pool file (404), a bad `Filename`, a wrong hash, an unsigned or half-swapped `dists/<suite>`.
- Data loss: a pool file or DB row removed that should not be — especially `gc` deleting an in-flight `add`'s file, or `check --purge-db` deleting a row over a hash mismatch.
- State leaking into the docroot: anything that puts `repo.db`/`config.yaml` under the public dir, or reads pool/dists from `Context.Root`.
- Path traversal or Release-header injection through scope names (`--distro`, `--suite`, `--component`, `--arch`) or `config.yaml` values.
- Non-atomic publish: `.new`/`.old` staging not cleaned, or a failure leaving the live suite broken.

## Blast radius
- Every downstream server running `apt update` against the repo. A bad publish is visible immediately to all of them.

## Reporting bar
- Report: anything above, missing transaction boundaries on multi-step writes, writer-lock gaps, validation bypasses at an entry point, tests that cannot tell the cases they claim apart.
- Do not report: style the linter (`.golangci.yml`, golangci-lint v2) does not enforce, missing comments (the project comments only the unexpected), speculative features.

## Deliberate conventions — not defects
- `delete` is DB-only and never unlinks pool files; orphans are reclaimed only by explicit `gc`. `publish` never auto-prunes.
- `check` is read-only by default; `--purge-db` reaps row-without-file only, never a hash mismatch. `gc` reaps file-without-row only.
- `gc` and `check --purge-db` hold the writer lock across the whole scan.
- `--distro` uses `config.ValidateDistroName` (one `/` allowed); other scope flags use `validateScopeName` (no `/`). Distro grammar is intentionally stricter than suite grammar.
- `--replace` on `add` takes one `.deb` only.
- `REPOKIT_PUBLIC` is merged by go-flags; never re-read with `os.Getenv`.
- `DistroName.Complete` stays silent on error (stderr would corrupt shell completion).
- The public dir is never auto-created; the cwd is never consulted for the state dir.
- Plans in `docs/plans/completed/` are immutable history, even when they drift from the code.
- Vendored deps (`vendor/`) are committed; one `_test.go` per source file.
