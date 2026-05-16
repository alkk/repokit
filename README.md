# repokit

Minimalistic Debian apt repository manager. Maintains static per-distro
`<distro>/pool/` + `<distro>/dists/` trees backed by SQLite, with explicit
publish-and-sign and atomic dist swaps.

Hashes (md5/sha1/sha256/sha512) are computed once at `add` time and persisted in
the database, so `publish` only re-emits them — unlike freight-style tools that
recompute on every run. Multiple distros and multiple suites per distro are
supported with isolated per-(distro, suite) pools (no cross-suite dedup).

## Build

```
make build
```

The resulting binary lands in `.bin/repokit`. The build embeds a revision string
derived from `git describe`; use `make version` to inspect what would be
embedded.

## Install

Copy `.bin/repokit` to a directory on `PATH` (e.g. `/usr/local/bin`). The binary
is self-contained — no runtime dependencies beyond `gpg` (only required when
signing).

Signing is delegated to `gpg-agent`. `repokit` never prompts for a passphrase —
provision the signing key in `gpg-agent` and confirm `gpg --list-secret-keys`
shows it before running `publish` without `--no-sign`.

## Development

| Target | What it does |
|---|---|
| `make test` | race detector + coverage (mocks excluded from the report) |
| `make lint` | `golangci-lint run` against `.golangci.yml` |
| `make fmt` | `gofmt -s` + `goimports` over non-vendor non-mock sources |
| `make race` | quick race-detector sweep with a 60s timeout |

## Commands

| Command | Purpose |
|---|---|
| `init` | initialize a new repository |
| `add` | stage one or more `.deb` files into a suite |
| `delete` | remove packages from a suite |
| `list` | list packages matching filters |
| `search` | substring search by name or description |
| `publish` | rebuild and sign `<distro>/dists/` for one or more suites |
| `check` | rehash pool files and report mismatches/orphans (read-only); `--purge-db` deletes rows whose pool file is gone |
| `gc` | remove orphan pool files (files with no database row) |
| `info` | inspect a `.deb` without touching the repo |
| `completion` | print a shell-completion wiring script (`bash`, `zsh`, or `fish`) |

`delete` is database-only — it removes rows but leaves the pool files on
disk as orphans so the published indexes keep resolving until the next
`publish`. Reclaim that disk space with the explicit `repokit gc`. The old
`verify --gc` flag was removed and the surrounding command renamed (no
deprecation alias): replace `repokit verify --gc` with `repokit gc`, and
`repokit verify` (no flag) with `repokit check`.

See [docs/usage.md](docs/usage.md) for the full flag reference.

## Shell completion

`repokit completion <shell>` prints a small wiring script for `bash`, `zsh`,
or `fish` to stdout. Source it once and TAB completes subcommands, flag
names, `.deb` file paths, and `--distro` values pulled live from
`config.yaml`.

```sh
# bash — in ~/.bashrc:
source <(repokit completion bash)

# zsh — write to a directory on $fpath and reload compinit:
repokit completion zsh > "${fpath[1]}/_repokit"

# fish:
repokit completion fish > ~/.config/fish/completions/repokit.fish
```

## Layout

`repokit` separates **state** (private to the operator) from the **public**
apt tree (served by your webserver):

| Dir | Holds | Default | Override |
|---|---|---|---|
| state dir | `repo.db`, `config.yaml` | `~/.repokit/` | `--repo` / `$REPOKIT_ROOT` |
| public dir | `<distro>/pool/`, `<distro>/dists/` | none — operator must set | `--public` / `$REPOKIT_PUBLIC` / `public_dir` in `config.yaml` |

Point your webserver's docroot at the public dir. The state dir holds the
signing config and SQLite database — never serve it.

The state dir is resolved from `--repo` > `$REPOKIT_ROOT` > `~/.repokit/`. The
current working directory is **not** consulted. Every command except `init`
and `info` requires a `repo.db` in the resolved state dir; commands that
touch the public tree (`add`, `publish`, `check`, `gc`)
additionally require the public dir to be set and to exist. `delete` is
database-only and runs without a resolvable public dir.

## Quickstart

```sh
# 1. Initialize state at the default ~/.repokit/
repokit init

# 2. Create the public tree (repokit does not auto-create it)
sudo mkdir -p /srv/apt
sudo chown "$USER" /srv/apt

# 3. Edit ~/.repokit/config.yaml — set signing_key to a fingerprint your gpg
#    agent has the secret key for, set public_dir to the directory above
#    (or pass --public /srv/apt on every command, or export REPOKIT_PUBLIC),
#    and list the distros you intend to manage under distros:.

# 4. Stage some packages
repokit add --distro debian --suite bookworm \
    ./hello_1.0_amd64.deb ./world_2.1_amd64.deb

# 5. Publish: build dists/, sign Release, atomically swap into place
repokit publish --distro debian --suite bookworm

# 6. Point a webserver docroot at /srv/apt and you're done.
```

## Consuming the repo with apt

On any client that should install from the repo, fetch and trust the public
half of the signing key, then add a sources entry. The URI must end in the
distro segment (`/<distro>`) — apt resolves `dists/<suite>/Release` against
that base, and on disk each distro is its own apt repo root.

For a repo published at `https://apt.example.com/` containing the `debian`
distro:

```sh
# Trust the signing key (replace with your key fingerprint / file)
sudo curl -fsSL https://apt.example.com/pubkey.asc \
    -o /etc/apt/keyrings/example.asc
```

deb822 form (`/etc/apt/sources.list.d/example.sources`):

```
Types: deb
URIs: https://apt.example.com/debian
Suites: bookworm
Components: main
Signed-By: /etc/apt/keyrings/example.asc
```

Or the equivalent one-line legacy form (`/etc/apt/sources.list.d/example.list`):

```
deb [signed-by=/etc/apt/keyrings/example.asc] https://apt.example.com/debian bookworm main
```

Then:

```sh
sudo apt-get update
sudo apt-get install hello
```

The tree is fully static — any HTTPS server (S3, nginx, GitHub Pages) will do.

## Configuration

The state-dir `config.yaml` lists the distros repokit is allowed to manage.
Every `--distro` argument to `add`, `delete`, `publish`, `check`, and `gc`
must appear in this list; an unconfigured value is rejected before any pool
or dists write. The public dir may hold unrelated top-level directories — those
are never walked.

```yaml
distros:
  - debian
  - ubuntu
  - customer/acme

signing_key: "DEADBEEF1234..."
public_dir: "/srv/apt"
```

Distro names may be plain (`debian`) or one-level namespaced
(`customer/acme`), producing a nested
`<publicDir>/customer/acme/{pool,dists}/` subtree. Depth ≥ 2 slashes is
rejected, as is a plain-prefix conflict like `[customer, customer/acme]`.

See [docs/usage.md](docs/usage.md) for the full config schema including
per-suite `Release` header overrides.

## Documentation

- [docs/usage.md](docs/usage.md) — full command reference, config schema, and
  on-disk layout.

## Limitations (v1)

- No `Contents-<arch>` files (used by `apt-file`, not `apt-get` itself).
- No `by-hash/` directories — `Acquire-By-Hash: no` is set explicitly in
  `Release`.
- No `Sources` indexes — `repokit` is binary-only.
- No `Packages.zst` — gz + xz is sufficient for every current apt client.

## License

MIT — see [LICENSE](LICENSE).
