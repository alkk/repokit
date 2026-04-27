# repokit usage

Reference documentation for the `repokit` command-line tool. This page is
information-oriented — for a guided walkthrough, see the quickstart in
[README.md](../README.md).

## Global flags

These apply to every subcommand and must precede the subcommand name.

| Flag | Env | Default | Description |
|---|---|---|---|
| `-r`, `--repo <path>` | `REPOKIT_ROOT` | current working directory | repository root |
| `-q`, `--quiet` | — | off | suppress non-error output |
| `-h`, `--help` | — | — | print help for the parser or active subcommand |

Repo-root resolution is `--repo` > `$REPOKIT_ROOT` > cwd. Every subcommand
except `init` and `info` requires the resolved root to contain a `repo.db`.

Scope flag values (`--distro`, `--suite`, `--component`, `--arch`) are
validated against path-traversal and Release-injection vectors. They must not
be `.` or `..`, must not start with `.` or `-`, and must not contain `/`,
`\`, NUL, spaces, or control characters. Suite names additionally must not
end in `.new` or `.old` — those suffixes are reserved by `publish` for
staging siblings.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | runtime error (I/O, gpg, hash mismatch, etc.) |
| `2` | usage error (bad flag combination, missing required argument) |

## Subcommands

### `init`

Create a new repository. Writes an empty `repo.db` with the current schema and
a starter `config.yaml` at the repo root. Per-distro `pool/` and `dists/`
subtrees are created on demand by the first `add` and `publish`.

```
repokit init [<repo-root>]
```

| Argument | Description |
|---|---|
| `<repo-root>` (positional, optional) | overrides `--repo` for this invocation |

Errors if `<root>/repo.db` already exists. A pre-existing `config.yaml` is left
untouched.

### `add`

Stage one or more `.deb` files into a `(distro, suite, component)` target.
Mutates `repo.db` and `pool/` only — running `publish` afterwards is required
to update the apt indexes.

```
repokit add --distro <d> --suite <s> [--component <c>] [--replace] <deb>...
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--distro <d>` | yes | — | target distribution (e.g. `debian`, `ubuntu`) |
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
| `--distro <d>` | yes | — | target distribution |
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
| `--distro <d>` | — | filter by distribution |
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
| `--distro <d>` | — | target distribution (must be paired with `--suite`) |
| `--suite <s>` | — | target suite (must be paired with `--distro`) |
| `--no-sign` | off | skip signing — produce stub `InRelease`/`Release.gpg` |

Both scope flags omitted means publish every suite known to the repository —
every suite with at least one row in the packages table, plus every suite that
already has a live `<distro>/dists/<suite>/Release` on disk. The filesystem
half ensures that a suite emptied by `delete` is republished with empty
indexes so apt clients stop seeing the stale `Packages`/`Release` files. Top-
level repo-root entries that aren't directories or whose names fail
distro-name validation, and directories that don't contain a `dists/`
subdirectory, are skipped — so stray files (`repo.db`, `config.yaml`,
`pubkey.asc`) and unrelated user-created dirs don't trigger spurious distros.
Staging siblings (`<suite>.new`, `<suite>.old`) and directories without a
`Release` file under `<distro>/dists/` are likewise skipped.

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
| `--distro <d>` | — | limit to one distribution |
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
the repo. Does not require a `repo.db` at the resolved root.

```
repokit info [--distro <d> --suite <s> [--component <c>]] <deb>
```

| Argument | Description |
|---|---|
| `<deb>` (positional, required) | path to the `.deb` file |

Prints `Package`, `Version`, `Architecture`, `Source` (when present), `Size`,
the four hashes, and the target pool path (when `--distro` and `--suite` are
supplied — otherwise a hint is printed).

## Configuration file

`config.yaml` lives at the repo root and is read by `publish`. A starter
template is written by `init`.

```yaml
signing_key: "DEADBEEF1234..."   # gpg key id or fingerprint (required)

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
```

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `signing_key` | string | yes | — | gpg key id or fingerprint that signs `Release` |
| `suites` | map | no | `{}` | per-suite Release header overrides keyed by `<distro>/<suite>` |
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

## On-disk layout

Each `<distro>` subtree is a self-contained apt repo root: an apt client
points `URI` at `<server-root>/<distro>` and resolves
`dists/<suite>/Release` relative to that base.

```
<repo-root>/
├── repo.db                     # SQLite — sole source of truth for metadata (shared)
├── config.yaml                 # signing key + per-suite Release overrides (shared)
└── <distro>/                   # one subtree per distro — its own apt repo root
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
omits the leading `<distro>` segment and reads
`pool/<suite>/<component>/<letter>/<source>/<pkg>_<ver>_<arch>.deb`. Apt
joins it onto the configured `URI` (which already ends in `/<distro>`) to
form the download URL.

Pools are isolated per `(distro, suite)`: the same NVRA added to two suites of
the same distro produces two separate physical files at
`<root>/<distro>/pool/<suite>/...` with identical content. There is no
cross-suite dedup.

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
    filename      TEXT NOT NULL,         -- apt-relative path; no distro prefix (relative to <root>/<distro>)
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
