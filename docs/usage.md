# repokit usage

Reference documentation for the `repokit` command-line tool. This page is
information-oriented — for a guided walkthrough, see the quickstart in
[README.md](../README.md).

## Global flags

These apply to every subcommand and must precede the subcommand name.

| Flag | Env | Default | Description |
|---|---|---|---|
| `-r`, `--repo <path>` | `REPOKIT_ROOT` | `~/.repokit/` | path to the state dir (`repo.db` + `config.yaml`) |
| `-p`, `--public <path>` | `REPOKIT_PUBLIC` | from `public_dir` in `config.yaml` | path to the public-facing tree (`<distro>/pool/` + `<distro>/dists/`) |
| `-q`, `--quiet` | — | off | suppress non-error output |
| `-h`, `--help` | — | — | print help for the parser or active subcommand |

`repokit` keeps two independent paths:

- **State dir** — holds `repo.db` and `config.yaml`. Resolution chain:
  `--repo` > `$REPOKIT_ROOT` > `~/.repokit/`. The current working directory
  is **not** consulted. Every subcommand except `init` and `info` requires
  the resolved state dir to contain a `repo.db`.
- **Public dir** — holds the per-distro `pool/` and `dists/` subtrees that a
  webserver will expose. Resolution chain: `--public` > `$REPOKIT_PUBLIC` >
  `public_dir` field in `<state-dir>/config.yaml`. Required for `add`,
  `publish`, `check`, and `gc`; ignored by `init`, `info`, `list`,
  `search`, and `delete` (`delete` is DB-only — it removes rows but never
  reads or writes pool/dists, so an unset `public_dir` cannot block it). The path must be **absolute**, must already **exist** as a
  directory (`repokit` does not create it), must not contain control or NUL
  characters, and must not equal or be nested with the state dir in either
  direction (a leak would put `repo.db` and `config.yaml` under the
  webserver's docroot).

When `public_dir` is unset (or still the `REPLACE_ME` placeholder) and no
flag/env override is given, the affected commands fail with:

```
public_dir is not set: edit <state-dir>/config.yaml or pass --public / $REPOKIT_PUBLIC
```

Scope flag values (`--distro`, `--suite`, `--component`, `--arch`) are
validated against path-traversal and Release-injection vectors. They must not
be `.` or `..`, must not start with `.` or `-`, and must not contain `\`,
NUL, spaces, or control characters. `--suite`, `--component`, and `--arch`
are single path segments and additionally reject `/`; `--suite` must also not
end in `.new` or `.old` — those suffixes are reserved by `publish` for
staging siblings. `--distro` may contain at most one `/` so it can name a
namespaced distro (`customer/acme`, or a versioned one like `7.x/debian`) —
each side of the slash is held to the `[a-zA-Z0-9.-]+` distro alphabet, must
be non-empty, must not start with `-` or `.`, must not end with `.`, and must
not contain `..`. Whatever `--distro` resolves to must also appear in `config.yaml`'s
`distros:` list for `add`, `delete`, `publish`, `check`, and `gc`.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | runtime error (I/O, gpg, hash mismatch, etc.) |
| `2` | usage error (bad flag combination, missing required argument) |

## Subcommands

### `init`

Create a new state dir. Writes an empty `repo.db` with the current schema and
a starter `config.yaml` at the resolved state dir (default `~/.repokit/`).
The state dir itself is created if missing. The public dir and per-distro
`pool/` and `dists/` subtrees are **not** touched by `init` — operators must
create the public dir manually and either set `public_dir` in the generated
`config.yaml` or pass `--public` / `$REPOKIT_PUBLIC` to subsequent commands.

```
repokit init [<state-dir>]
```

| Argument | Description |
|---|---|
| `<state-dir>` (positional, optional) | overrides `--repo` and `$REPOKIT_ROOT` for this invocation |

Errors if `<state-dir>/repo.db` already exists. A pre-existing `config.yaml`
is left untouched. The starter `config.yaml` contains `signing_key:
"REPLACE_ME"` and `public_dir: "REPLACE_ME"` — both must be edited before
`publish` (without `--no-sign`) and any `add` / `delete` / `publish` /
`check` / `gc` invocation respectively will succeed.

### `add`

Stage one or more `.deb` files into a `(distro, suite, component)` target.
Mutates `repo.db` and `pool/` only — running `publish` afterwards is required
to update the apt indexes.

```
repokit add --distro <d> --suite <s> [--component <c>] [--replace | --skip-existing] <deb>...
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--distro <d>` | yes | — | target distribution — plain (`debian`) or one-level namespaced (`customer/acme`); must be declared in `config.yaml`'s `distros:` |
| `--suite <s>` | yes | — | target suite (e.g. `bookworm`) |
| `--component <c>` | no | `main` | target component |
| `--replace` | no | off | overwrite an existing same-NVRA package |
| `--skip-existing` | no | off | skip packages whose name/version/architecture is already present in the target (mutually exclusive with `--replace`) |

The whole batch is wrapped in a single transaction. On any failure (duplicate
without `--replace` or `--skip-existing`, parse error, I/O error) the database
rolls back and pool files written earlier in the batch are deleted in reverse
order.

`--replace` accepts only a single `.deb` per invocation, since overwriting a
pool file cannot be safely rolled back if a later item in the batch fails.

A duplicate without `--replace` or `--skip-existing` prints the existing-vs-new
sha256 diff before exiting with code 1.

Re-adding a package that was previously `delete`d works without `--replace`:
because `delete` is DB-only, the prior pool file survives as an orphan, and a
plain `add` of the byte-identical `.deb` re-adopts that file in place and
re-inserts the row. Only a pool file whose bytes **differ** from the incoming
`.deb` (and has no DB row) still hard-errors with `pool: <path> already
exists` — `--replace` remains the only way to clobber differing content.

`--skip-existing` makes batch re-adds idempotent: each duplicate NVRA is logged
on stderr as `skipping <name>_<ver>_<arch>: already present in
<distro>/<suite>/<component>` and the batch continues. The summary becomes
`staged N packages, skipped M` when any were skipped. When the duplicate NVRA's
stored sha256 differs from the incoming `.deb`'s sha256, the line is upgraded
to `warning: skipping ...: NVRA matches but sha256 differs (existing=<sha>
new=<sha>) — use --replace to overwrite` so "same NVRA, different bytes"
footguns surface even under `--quiet` (the warning stays on stderr; only the
trailing stdout summary is suppressed). `--skip-existing` and `--replace`
cannot be combined.

A `.deb` whose `control.tar.*` `control` member is larger than 1 MiB or
contains an embedded blank line (which would split into multiple stanzas in
the published `Packages` index) is rejected before any pool write or DB
insert.

### `delete`

Remove packages from a suite. Mutates `repo.db` **only** — it deletes the
matching `packages` rows and never touches the `pool/` tree. Running
`publish` afterwards is required to remove the entries from the apt indexes.
A subsequent `publish` (with explicit `--distro`/`--suite` or both omitted)
rebuilds `<distro>/dists/<suite>/` even when `delete` emptied the suite —
empty `Packages` files and a `Release` with empty
`Architectures:`/`Components:` are written so apt clients stop seeing the
removed packages.

Because `delete` leaves the pool files in place, the published indexes keep
resolving every `.deb` they list right up until the next `publish` — apt
clients hitting the repo in the `delete`→`publish` window get no `404`s. The
now-unreferenced files become **orphans** and are reclaimed only by an
explicit `repokit gc` (see below); `publish` never prunes them.

```
repokit delete --distro <d> --suite <s> [filters...] [--all-versions] [--dry-run]
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--distro <d>` | yes | — | target distribution — plain (`debian`) or one-level namespaced (`customer/acme`); must be declared in `config.yaml`'s `distros:` |
| `--suite <s>` | yes | — | target suite |
| `--component <c>` | no | — | filter by component |
| `--arch <a>` | no | — | filter by architecture |
| `--name <n>` | no\* | — | filter by package name |
| `--version <v>` | no | — | filter by package version |
| `--sha256 <hex>` | no\* | — | filter by sha256 |
| `--all-versions` | no | off | required when `--name` is set without `--version` |
| `--dry-run` | no | off | print what would be deleted; make no changes |

\* At least one of `--name` or `--sha256` is required to prevent a typo from
wiping a whole suite.

`delete` performs no pool I/O at all — there is no unlink and no
empty-parent prune. The matching rows are removed inside a single
writer-locked transaction; pruning the disk bytes those rows pointed at is
deferred to `repokit gc`, which walks the pool tree and removes everything
no row references.

### `list`

Print packages matching the supplied filters.

```
repokit list [filters...] [--format table|json]
```

| Flag | Default | Description |
|---|---|---|
| `--distro <d>` | — | filter by distribution (plain or namespaced) |
| `--suite <s>` | — | filter by suite |
| `--component <c>` | — | filter by component |
| `--arch <a>` | — | filter by architecture |
| `--name <n>` | — | filter by package name |
| `--format table\|json` | `table` | output format |

The `table` format produces aligned columns
`DISTRO SUITE COMPONENT ARCH NAME VERSION SIZE` with a header row. The `json`
format emits one JSON object per row (line-delimited, jq-friendly). Empty
result sets produce no output in either format.

### `search`

Substring match (case-insensitive) against the package name and the stored
control text.

```
repokit search [--distro <d>] [--suite <s>] [--format table|json] <query>
```

| Argument | Description |
|---|---|
| `<query>` (positional, required) | substring to match |

Output formats are identical to `list`. Search is a linear scan; performance is
acceptable at thousands of packages but degrades at six-figure scale.

### `publish`

Rebuild `<distro>/dists/<suite>/` for one or more scopes and sign the resulting
`Release`.

```
repokit publish [--distro <d> --suite <s>] [--no-sign]
```

| Flag | Default | Description |
|---|---|---|
| `--distro <d>` | — | target distribution — plain or one-level namespaced; must be paired with `--suite` |
| `--suite <s>` | — | target suite (must be paired with `--distro`) |
| `--no-sign` | off | skip signing — produce stub `InRelease`/`Release.gpg` |

Both scope flags omitted means publish every suite known to the repository,
constrained to the distros listed in `config.yaml`'s `distros:` field — every
suite with at least one row in the packages table for a configured distro,
plus every suite that already has a live `<configured-distro>/dists/<suite>/Release`
on disk. The filesystem half ensures that a suite emptied by `delete` is
republished with empty indexes so apt clients stop seeing the stale
`Packages`/`Release` files. Unrelated top-level directories under the public
dir are never walked. Staging siblings (`<suite>.new`, `<suite>.old`) and
directories without a `Release` file under `<distro>/dists/` are skipped.

Specifying one scope flag without the other is a usage error.

For each scope, publish:

1. Cleans any leftover `<suite>.new` and `<suite>.old` from a prior crashed run.
2. Builds `<distro>/dists/<suite>.new/` with one Packages triple
   (`Packages`/`Packages.gz`/`Packages.xz`) per `(component, concrete-arch)`
   pair present in the DB. `arch=all` rows are included in every concrete
   `binary-<arch>/Packages` (apt convention — no `binary-all/` directory).
3. Writes `Release` listing all three Packages variants per
   `(component, arch)`.
4. Signs `Release` (unless `--no-sign`), producing `InRelease` (clearsigned)
   and `Release.gpg` (detached, ASCII-armored).
5. Atomically swaps `<suite>.new` into place: `live → <suite>.old`,
   `<suite>.new → live`, `rm -rf <suite>.old`. Parent directory is fsynced
   between renames.
6. Updates `meta.last_publish_at` in `repo.db`.

Pool files are never touched by publish.

### `check`

Rehash pool files against the four hashes stored in the database and report
inconsistencies. `check` is strictly **read-only by default**; the optional
`--purge-db` flag promotes it to a writer-locked drift-resolver that deletes
rows whose pool file has gone missing.

```
repokit check [--distro <d>] [--suite <s>] [--purge-db]
```

| Flag | Default | Description |
|---|---|---|
| `--distro <d>` | — | limit check to one distribution (plain or namespaced) |
| `--suite <s>` | — | limit check to one suite (requires `--distro`) |
| `--purge-db` | off | delete database rows whose pool file is missing on disk (mismatches are left alone) |

For every `packages` row in scope, the referenced pool file is re-hashed and
compared against the stored md5/sha1/sha256/sha512. The pool subtree in scope
is then walked for files no row references.

Output lines are `mismatch <path>` (stored hashes disagree with the file on
disk), `missing <path>` (a row points at a pool file that is gone), and
`orphan <path>` (a pool file with no DB row), followed by either `check: ok`
or a `check: N mismatch(es), M missing, K orphan(s)` summary. Findings are
always printed; `--quiet` suppresses only the trailing status line. Exit code
is 0 when the scope is clean (no mismatches, no missing files, no orphans),
1 otherwise.

Without `--purge-db`, `check` does not act on any finding. An `orphan` is the
operator's signal to run `repokit gc`; a `missing` row, or a `mismatch`,
requires a manual decision (`check` never deletes a row pointing at a missing
or wrong file). Because `delete` is DB-only, an orphan is the normal,
expected state of a just-deleted-but-not-yet-`gc`'d package — `check`
flagging it (and exiting non-zero) is by design, not corruption.

Files under `pool/` whose names end in `.tmp` or `.bak` are transient
artifacts of an in-flight or crashed `add`. They are never reported as
orphans by `check` (and never removed by `gc`) — that would destroy the
only recoverable copy after an interrupted `--replace`. They are cleaned
up automatically by the next successful write at the same path.

#### `--purge-db`

`--purge-db` resolves the mirror image of an orphan: a `packages` row whose
pool file has been removed out-of-band. Each such row is deleted and its
report line changes from `missing <path>` to `purged <path>`. The whole
pass — the row snapshot, the hash check, the deletes, and the orphan walk —
runs under the same writer lock `gc` uses, so a concurrent `add` cannot have
its in-flight pool file misclassified or slip a row past the deletes.

The contract is **missing-only**: a hash `mismatch` is never auto-resolved.
A transient FS/mount glitch can make a present file look corrupt, and
silently dropping the row for a "corrupt" package would mask a real problem,
so mismatches are still only reported. `gc` reaps file-without-row;
`check --purge-db` reaps row-without-file.

In purge mode the summary is always printed (even for a no-op run) and reads
`check: N mismatch(es), M purged, K orphan(s)`. The exit code follows the
**post-action** state: 0 iff no mismatches and no orphans remain after the
purge — a successfully purged row does not fail the run (mirroring `gc`'s
"the action that resolves the finding does not fail the run" rule), but an
unresolved mismatch or a real orphan still exits 1. There is no `--dry-run`:
preview exactly what `--purge-db` would remove by running plain `check`
first and reading the `missing` lines.

`--purge-db` honours the existing `--distro` / `--suite` scope filters;
there is no separate purge-scope flag. Each delete is keyed by the full row
identity `(distro, suite, component, architecture, name, version)`. Pool
paths embed the suite and component, so the same package in two suites is
two distinct files backing two distinct rows — never one shared file.
Deleting by the full key purges exactly the rows whose file was verified
missing and leaves an intact file's row in another suite untouched.

### `gc`

Remove orphan pool files: regular files under `<distro>/pool/...` that no
`packages` row references. `gc` is the **only** command that deletes pool
files. It never touches the database — `delete` retires rows; `gc` later
reclaims the disk bytes those retired rows used to point at.

```
repokit gc [--distro <d>] [--suite <s>] [--dry-run]
```

| Flag | Default | Description |
|---|---|---|
| `--distro <d>` | — | limit gc to one distribution (plain or namespaced) |
| `--suite <s>` | — | limit gc to one suite (requires `--distro`) |
| `--dry-run` | off | print the orphans that would be removed; make no changes |

`gc` is deliberately orphan-only and existence-based: there are no
`--name` / `--sha256` surgical filters (that is `delete`'s job) and no
hashing (that is `check`'s job — `gc` only needs the set of pool paths the
DB still knows). It also never deletes a DB row, so a row pointing at a
missing pool file is left for `check --purge-db` (or a manual `delete`).

Output is one `removed <path>` line per orphan (`would remove <path>` under
`--dry-run`), followed by a `gc: removed N orphan(s)` (or
`gc: would remove N orphan(s)`) summary. Per-orphan lines are always printed;
`--quiet` suppresses only the trailing summary. After each orphan is removed,
empty ancestor directories are pruned up to — but not including —
`<distro>/pool/`, mirroring the old `delete` behavior. Prune failures are
non-fatal and surface on stderr as `prune: <error>`.

Orphan presence is **not** a failure — `gc` is the action that resolves it —
so both a clean run and a run that removed orphans exit 0. A real removal
takes a writer lock spanning the scan and the unlink so a concurrent `add`
(which writes pool bytes before its row commits) cannot have its in-flight
file misclassified as an orphan and deleted. `--dry-run` takes no lock
(nothing is removed, so a snapshot read is safe) and never modifies disk.

`--suite` requires `--distro` (the pool tree is rooted per distro, so a bare
`--suite` has no single subtree to walk); supplying `--suite` without
`--distro` is a usage error (exit 2). Whatever `--distro` resolves to must appear in
`config.yaml`'s `distros:` list; an unconfigured distro aborts with exit 1.
When neither scope flag is given, `gc` walks the pool of every configured
distro and leaves unrelated top-level directories under the public dir
untouched.

> **Migrating from `verify --gc`:** the `--gc` flag was removed and the
> command itself renamed (pre-1.0, no deprecation alias). Replace
> `repokit verify --gc` with `repokit gc`, and
> `repokit verify --gc --distro X --suite Y` with
> `repokit gc --distro X --suite Y`. The read-only report formerly spelled
> `repokit verify` (no flag) is now `repokit check`; to also resolve
> missing-pool drift use `repokit check --purge-db`.

### `info`

Inspect a single `.deb` file and print what `add` would do, without touching
the repo. Does not require a `repo.db` at the resolved state dir.

```
repokit info [--distro <d> --suite <s> [--component <c>]] <deb>
```

| Argument | Description |
|---|---|
| `<deb>` (positional, required) | path to the `.deb` file |

Prints `Package`, `Version`, `Architecture`, `Source` (when present), `Size`,
the four hashes, and the target pool path (when `--distro` and `--suite` are
supplied — otherwise a hint is printed).

### `completion`

Print a shell wiring script that delegates TAB completion for `repokit` back
to the binary itself (via `GO_FLAGS_COMPLETION=1`). The script is written to
stdout; source it from your shell rc to activate. Does not read or require a
`repo.db`, `config.yaml`, or `public_dir` — safe to run on a fresh machine
before `repokit init`.

```
repokit completion <shell>
```

| Argument | Description |
|---|---|
| `<shell>` (positional, required) | one of `bash`, `zsh`, `fish` |

Install examples:

```sh
# bash — add to ~/.bashrc:
source <(repokit completion bash)

# zsh — write to a directory on $fpath and reload compinit:
repokit completion zsh > "${fpath[1]}/_repokit"

# fish:
repokit completion fish > ~/.config/fish/completions/repokit.fish
```

Once sourced, TAB completes subcommand names, flag names, `.deb` file paths,
and `--distro` values drawn live from `config.yaml`'s `distros:` list.

Dynamic `--distro` completion resolves the state dir using the same chain as
every other command (`--repo` > `$REPOKIT_ROOT` > `~/.repokit/`), reading
`--repo` from the partial command line being completed. If the resolved
state dir has no `config.yaml`, is unreadable, or holds a malformed config,
TAB silently produces no values — run the equivalent non-completion command
to surface the underlying error.

Unsupported shell values exit with code `2` and print the accepted list to
stderr. The wiring scripts are embedded in the binary at build time, so no
filesystem layout assumptions apply.

## Configuration file

`config.yaml` lives in the state dir (the same directory as `repo.db`) and is
read by every command that resolves a public dir or signs `Release`. A
starter template is written by `init`.

```yaml
distros:                          # managed distros — every --distro flag must
  - debian                        # appear here; unconfigured values are rejected
  - ubuntu
  - customer/acme                 # one-level namespaced distros are allowed

signing_key: "DEADBEEF1234..."   # gpg key id or fingerprint (required for signed publish)
public_dir: "/srv/apt"           # absolute path the webserver docroot points at

suites:
  debian/bookworm:
    origin: "MyRepo"
    label: "MyRepo"
    suite: "bookworm"
    codename: "bookworm"
    description: "Internal builds for Debian 12"
  debian/bullseye:
    origin: "MyRepo"
    # remaining fields default from the key
  customer/acme/stable:           # namespaced distro → three-segment key
    origin: "Acme"
```

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `distros` | list of strings | yes for add/delete/publish/check/gc | `[]` | distros repokit is allowed to manage; each entry is either a plain `[a-zA-Z0-9.-]+` segment (no leading `-` or `.`, no trailing `.`, no `..`) or two such segments joined by exactly one `/` (`customer/acme`, `7.x/debian`); no duplicates, no entry deeper than one `/`, a plain entry may not share its top-level segment with a namespaced one (`[customer, customer/acme]` is rejected), and every `suites:` key's distro half must appear here |
| `signing_key` | string | yes (unless `--no-sign`) | — | gpg key id or fingerprint that signs `Release` |
| `public_dir` | string | yes for add/publish/check/gc (unless `--public` / `$REPOKIT_PUBLIC` is set); not needed by DB-only `delete` | — | absolute path to the public-facing tree |
| `suites` | map | no | `{}` | per-suite Release header overrides keyed by `<distro>/<suite>` (a namespaced distro yields a three-segment key like `customer/acme/stable` — the key is split on its **last** `/`) |
| `suites[k].origin` | string | no | `<distro>` | `Origin:` field in `Release` |
| `suites[k].label` | string | no | `<distro>` | `Label:` field in `Release` |
| `suites[k].suite` | string | no | `<suite>` | `Suite:` field in `Release` |
| `suites[k].codename` | string | no | `<suite>` | `Codename:` field in `Release` |
| `suites[k].description` | string | no | `<distro> <suite>` | `Description:` field in `Release` |

Suites not declared in the config still publish — every header field falls back
to its default, derived from the `(distro, suite)` pair.

`signing_key` must be set to a real gpg key id or fingerprint before `publish`
can sign. The starter template written by `init` uses `REPLACE_ME` as a
placeholder — leaving it as-is and running `publish` without `--no-sign` will
pass `REPLACE_ME` to `gpg --default-key` verbatim, which fails with a gpg-side
error. Pass `--no-sign` for unsigned test repos.

`public_dir` is treated as unset when missing or left as `REPLACE_ME`. The
flag and env var (`--public` / `$REPOKIT_PUBLIC`) take precedence — useful
for one-off invocations against a non-default tree without editing
`config.yaml`.

## On-disk layout

`repokit` keeps the operator's private state and the apt-facing public tree
in two **separate directories** so the webserver docroot can point at the
public tree without exposing `repo.db` or the signing config. Each `<distro>`
subtree under the public dir is a self-contained apt repo root: an apt client
points `URI` at `<server-root>/<distro>` and resolves `dists/<suite>/Release`
relative to that base. A namespaced distro like `customer/acme` simply expands
`<distro>` to the nested path `customer/acme/` — the apt `URI` becomes
`<server-root>/customer/acme`, and `Filename:` in `Packages` still omits the
entire `<distro>` prefix (clients get it from the `URI` base, not the field).

State dir (`--repo` / `$REPOKIT_ROOT` / `~/.repokit/`):

```
<state-dir>/
├── repo.db        # SQLite — sole source of truth for metadata (shared)
└── config.yaml    # signing key, public_dir, per-suite Release overrides
```

Public dir (`--public` / `$REPOKIT_PUBLIC` / `public_dir` in config.yaml) —
**this is what the webserver serves**:

```
<public-dir>/
└── <distro>/                   # one subtree per distro — its own apt repo root
    │                           # (a namespaced distro like customer/acme nests
    │                           #  one extra level: customer/acme/{pool,dists}/)
    ├── pool/
    │   └── <suite>/<component>/<letter>/<source>/<pkg>_<ver>_<arch>.deb
    └── dists/
        └── <suite>/
            ├── Release         # plain-text apt Release file
            ├── Release.gpg     # detached ASCII-armored signature
            ├── InRelease       # clearsigned Release (preferred by modern apt)
            └── <component>/binary-<arch>/
                ├── Packages
                ├── Packages.gz
                └── Packages.xz
```

`<letter>` is the first character of the source name (or package name, if the
control stanza has no `Source` field), lowercased. Names beginning with `lib`
use `lib<second-letter>` per Debian convention (e.g. `libfoo` →
`libf/libfoo/`).

The `Filename:` field emitted in each `Packages` index is apt-relative — it
omits the entire `<distro>` prefix (including any namespace segment) and reads
`pool/<suite>/<component>/<letter>/<source>/<pkg>_<ver>_<arch>.deb`. Apt
joins it onto the configured `URI` (which already ends in `/<distro>`) to
form the download URL.

Pools are isolated per `(distro, suite)`: the same NVRA added to two suites of
the same distro produces two separate physical files at
`<public-dir>/<distro>/pool/<suite>/...` with identical content. There is no
cross-suite dedup.

### Webserver docroot

Point your webserver at `<public-dir>` directly. nginx:

```nginx
server {
    listen 80;
    server_name apt.example.com;
    root /srv/apt;          # = public_dir
    autoindex on;           # optional — apt does not require directory listings
    location / {
        try_files $uri $uri/ =404;
    }
}
```

Apache 2.4:

```apache
<VirtualHost *:80>
    ServerName apt.example.com
    DocumentRoot /srv/apt
    <Directory /srv/apt>
        Require all granted
        Options +Indexes
    </Directory>
</VirtualHost>
```

Do **not** point a vhost at the state dir — `repo.db` and `config.yaml` are
not meant for public consumption. Keep the state dir somewhere the webserver
user has no read access (the `~/.repokit/` default already satisfies this for
typical setups where the webserver runs as `www-data`).

## Database schema

The schema is migration-managed; the current version is recorded in
`meta.schema_version`.

```sql
CREATE TABLE packages (
    id            INTEGER PRIMARY KEY,
    distro        TEXT NOT NULL,
    suite         TEXT NOT NULL,
    component     TEXT NOT NULL,
    architecture  TEXT NOT NULL,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    filename      TEXT NOT NULL,         -- apt-relative path; no distro prefix (resolves on disk to <public-dir>/<distro>/<filename>)
    size          INTEGER NOT NULL,
    md5           TEXT NOT NULL,
    sha1          TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    sha512        TEXT NOT NULL,
    control       TEXT NOT NULL,         -- verbatim normalised control stanza
    added_at      INTEGER NOT NULL,
    UNIQUE (distro, suite, component, architecture, name, version)
);
CREATE INDEX idx_pkg_lookup    ON packages(distro, suite, component, architecture);
CREATE INDEX idx_pkg_name_ver  ON packages(name, version);
CREATE INDEX idx_pkg_sha256    ON packages(sha256);

CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- known keys: schema_version, last_publish_at
```

Pragmas: `journal_mode=WAL`, `foreign_keys=ON`, `synchronous=NORMAL`.

## Deliberate v1 omissions

Documented so consumers aren't surprised when comparing against a Debian
mirror:

- No `Contents-<arch>` files (used by `apt-file`, not `apt-get` itself).
- No `by-hash/` directories. `Acquire-By-Hash: no` is set explicitly in
  `Release`.
- No `Sources` indexes — `repokit` is binary-only.
- No `Packages.zst` — gz + xz is sufficient for every current apt client.
