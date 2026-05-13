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
  `delete`, `publish`, and `verify`; ignored by `init`, `info`, `list`, and
  `search`. The path must be **absolute**, must already **exist** as a
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
namespaced distro (`customer/acme`) — each side of the slash is held to the
`[a-zA-Z0-9-]+` distro alphabet, must be non-empty, and must not start with
`-`. Whatever `--distro` resolves to must also appear in `config.yaml`'s
`distros:` list for `add`, `delete`, `publish`, and `verify`.

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
`verify` invocation respectively will succeed.

### `add`

Stage one or more `.deb` files into a `(distro, suite, component)` target.
Mutates `repo.db` and `pool/` only — running `publish` afterwards is required
to update the apt indexes.

```
repokit add --distro <d> --suite <s> [--component <c>] [--replace] <deb>...
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--distro <d>` | yes | — | target distribution — plain (`debian`) or one-level namespaced (`customer/acme`); must be declared in `config.yaml`'s `distros:` |
| `--suite <s>` | yes | — | target suite (e.g. `bookworm`) |
| `--component <c>` | no | `main` | target component |
| `--replace` | no | off | overwrite an existing same-NVRA package |

The whole batch is wrapped in a single transaction. On any failure (duplicate
without `--replace`, parse error, I/O error) the database rolls back and pool
files written earlier in the batch are deleted in reverse order.

`--replace` accepts only a single `.deb` per invocation, since overwriting a
pool file cannot be safely rolled back if a later item in the batch fails.

A duplicate without `--replace` prints the existing-vs-new sha256 diff before
exiting with code 1.

A `.deb` whose `control.tar.*` `control` member is larger than 1 MiB or
contains an embedded blank line (which would split into multiple stanzas in
the published `Packages` index) is rejected before any pool write or DB
insert.

### `delete`

Remove packages from a suite. Mutates `repo.db` and `pool/` only — running
`publish` afterwards is required to remove the entries from the apt indexes.
A subsequent `publish` (with explicit `--distro`/`--suite` or both omitted)
rebuilds `<distro>/dists/<suite>/` even when `delete` emptied the suite —
empty `Packages` files and a `Release` with empty
`Architectures:`/`Components:` are written so apt clients stop seeing the
removed packages.

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

### `verify`

Rehash pool files against the four hashes stored in the database, and (with
`--gc`) garbage-collect orphan pool files that have no DB row.

```
repokit verify [--distro <d>] [--suite <s>] [--gc]
```

| Flag | Default | Description |
|---|---|---|
| `--distro <d>` | — | limit to one distribution (plain or namespaced) |
| `--suite <s>` | — | limit to one suite (requires `--distro`) |
| `--gc` | off | remove orphan pool files |

Output lines are `mismatch <path>`, `missing <path>`, `orphan <path>` (or
`orphan <path> (removed)` under `--gc`), followed by either `verify: ok` or a
counts summary. Exit code is 0 when the scope is clean (after any `--gc`
removals), 1 otherwise.

`--gc` never deletes DB rows that point at a missing pool file — that
resolution requires a manual decision.

Files under `pool/` whose names end in `.tmp` or `.bak` are transient
artifacts of an in-flight or crashed `add`. They are not reported as orphans
— `--gc` would otherwise destroy the only recoverable copy after an
interrupted `--replace`. They are cleaned up automatically by the next
successful write at the same path.

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
| `distros` | list of strings | yes for add/delete/publish/verify | `[]` | distros repokit is allowed to manage; each entry is either a plain `[a-zA-Z0-9-]+` segment or two such segments joined by exactly one `/` (`customer/acme`); no duplicates, no entry deeper than one `/`, a plain entry may not share its top-level segment with a namespaced one (`[customer, customer/acme]` is rejected), and every `suites:` key's distro half must appear here |
| `signing_key` | string | yes (unless `--no-sign`) | — | gpg key id or fingerprint that signs `Release` |
| `public_dir` | string | yes for add/delete/publish/verify (unless `--public` / `$REPOKIT_PUBLIC` is set) | — | absolute path to the public-facing tree |
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
