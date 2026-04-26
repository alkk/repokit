# debrepo

Minimalistic Debian apt repository manager. Maintains a static `pool/` + `dists/`
tree backed by SQLite, with explicit publish-and-sign and atomic dist swaps.

Hashes (md5/sha1/sha256/sha512) are computed once at `add` time and persisted in
the database, so `publish` only re-emits them — unlike freight-style tools that
recompute on every run. Multiple distros and multiple suites per distro are
supported with isolated per-(distro, suite) pools (no cross-suite dedup).

## Build

```
make build
```

The resulting binary lands in `.bin/debrepo`. The build embeds a revision string
derived from `git describe`; use `make version` to inspect what would be
embedded.

## Install

Copy `.bin/debrepo` to a directory on `PATH` (e.g. `/usr/local/bin`). The binary
is self-contained — no runtime dependencies beyond `gpg` (only required when
signing).

Signing is delegated to `gpg-agent`. `debrepo` never prompts for a passphrase —
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
| `publish` | rebuild and sign `dists/` for one or more suites |
| `verify` | rehash pool files and (optionally) GC orphans |
| `info` | inspect a `.deb` without touching the repo |

See [docs/usage.md](docs/usage.md) for the full flag reference.

## Quickstart

```sh
# 1. Initialize a new repository
debrepo init /srv/apt

# 2. Edit /srv/apt/config.yaml and set signing_key to a fingerprint your gpg
#    agent has the secret key for (or pass --no-sign on publish for unsigned
#    test repos).

# 3. Stage some packages
debrepo --repo /srv/apt add --distro debian --suite bookworm \
    ./hello_1.0_amd64.deb ./world_2.1_amd64.deb

# 4. Publish: build dists/, sign Release, atomically swap into place
debrepo --repo /srv/apt publish --distro debian --suite bookworm

# 5. Rsync the static tree to the CDN (out of scope for debrepo)
rsync -av /srv/apt/{pool,dists}/ deploy@cdn:/var/www/apt/
```

The repo root is resolved from `--repo` (or `-r`), then `$DEBREPO_ROOT`, then
the current working directory. Every command except `init` and `info` requires a
`repo.db` at the resolved root.

## Consuming the repo with apt

On any client that should install from the repo, fetch and trust the public
half of the signing key, then add a sources entry. For a repo published at
`https://apt.example.com/`:

```sh
# Trust the signing key (replace with your key fingerprint / file)
sudo curl -fsSL https://apt.example.com/pubkey.asc \
    -o /etc/apt/keyrings/example.asc

# Add the source list
echo "deb [signed-by=/etc/apt/keyrings/example.asc] https://apt.example.com bookworm main" \
    | sudo tee /etc/apt/sources.list.d/example.list

sudo apt-get update
sudo apt-get install hello
```

The tree is fully static — any HTTPS server (S3, nginx, GitHub Pages) will do.

## Documentation

- [docs/usage.md](docs/usage.md) — full command reference, config schema, and
  on-disk layout.

## Limitations (v1)

- No `Contents-<arch>` files (used by `apt-file`, not `apt-get` itself).
- No `by-hash/` directories — `Acquire-By-Hash: no` is set explicitly in
  `Release`.
- No `Sources` indexes — `debrepo` is binary-only.
- No `Packages.zst` — gz + xz is sufficient for every current apt client.

## License

TBD.
