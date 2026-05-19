# Zensical documentation website

## Overview

Build a small static documentation website for repokit using
[Zensical](https://zensical.org) (squidfunk's successor to MkDocs Material).
The site is built locally with `uv run zensical build` and emits static HTML
under `website/site/`. The operator distributes the built tree however they
like — this plan adds **no** deployment plumbing, no CI, no rsync target.

The site exists alongside the existing GitHub-facing `README.md` (which stays
untouched) and goes deeper than the README: install details, full command
reference, configuration walkthrough, task-oriented how-tos, and an
architecture explainer.

### Brainstorm decisions that anchor this plan

1. **Approach**: minimal `website/` project at repo root. Not Diátaxis-formal,
   not a fresh README port, not a doc/site/ co-location. README stays as-is;
   site is independent and goes deeper.
2. **Hosting**: static output only. User deploys to their own infra (intended
   to land at the root of a dedicated domain). No GitHub Pages, no CI.
3. **Python tooling**: `uv` exclusively. `uv sync` for deps, `uv run zensical
   …` for build/serve. No `pip`, no `requirements.txt`.
4. **Theme**: Zensical default indigo, light+dark toggle, built-in search,
   GitHub link in header. One small override (`copyright.html`) to render
   `REPOKIT_VERSION` in the footer.
5. **Versioning**: static version baked at build time from the existing
   `$(REV)` Make variable (already `git describe`-derived). No multi-version
   selector.
6. **`site_url`**: left unset for now — relative links work at root. Set
   when a domain is chosen.
7. **Nav**: flat, six items (Home, Install, Commands, Configuration, How-to,
   Architecture). No nesting.

## Context (from discovery)

- `Makefile` — already computes `BRANCH`, `HASH`, `TIMESTAMP`, and the
  composite `REV` (single-string) via `git describe`. The `version:` target
  prints these for inspection. Plan reuses `$(REV)` directly as
  `REPOKIT_VERSION` env for Zensical (no new Make logic).
- `README.md` — 210 lines, stays as the GitHub front door. **Not modified by
  this plan.**
- `docs/usage.md` — 641 lines of per-command usage, the primary source to
  distill into `website/docs/commands.md`. **Not modified.**
- `CLAUDE.md` — architecture and gotcha notes; the primary source for
  `website/docs/architecture.md`. **Not modified.**
- `docs/plans/` — existing plans follow `yyyymmdd-<task>.md`; this plan
  lives at `docs/plans/20260519-zensical-docs-website.md`.
- `.gitignore` (root) — currently ignores `.bin/`, `.gcache/`, etc. Adds
  one line for `website/site/`.

Zensical specifics worth pinning down:

- Config file is `zensical.yml` (Zensical's convention, not `mkdocs.yml`).
- Supports `theme.custom_dir` for partial overrides — same mechanism as
  Material's `overrides/` tree.
- Built-in client-side search is on by default; no plugin needed.
- `extra:` table in config is exposed to Jinja templates as `config.extra`.
- Zensical requires Python ≥ 3.11.

## Development Approach

- **testing approach**: docs-site verification, not TDD. There is no
  application code to unit-test. Each task's "tests" are:
  1. `uv run zensical build` succeeds with no warnings escalated to errors
  2. `uv run zensical serve` renders the affected page correctly (manual
     spot check)
  3. Internal links resolve (Zensical's build emits warnings on broken
     refs — treat any new warning as a regression)
- complete each task fully before moving to the next
- make small, focused changes (one page or one config slice per task)
- **CRITICAL: every task ends with a clean `zensical build`** — no warnings
  about broken nav refs, broken internal links, or missing assets
- run `uv run zensical build` after each content task; visual-check via
  `uv run zensical serve` for layout/theme changes

## Testing Strategy

- **No unit tests** — there is no Go or Python code in this plan beyond
  configuration. The Go test suite (`make test`) is not affected.
- **Build-time checks**: `zensical build` is the smoke test. A broken nav
  entry, a missing markdown file, or a 404'd internal link fails the build.
- **Manual visual check**: `uv run zensical serve` on `http://127.0.0.1:8000`
  after each page lands. Verify:
  - page renders, headings are correct depth
  - code blocks have the right language hint and copy button
  - dark mode toggle works
  - GitHub header icon links out correctly
  - footer shows the injected version string
- **No e2e tests** — this is a static doc site with no interactive UI
  components beyond Zensical's own theme. Adding Playwright here would be
  pure ceremony.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

A single Zensical project under `website/` with `uv`-managed deps. Six
markdown pages, default indigo theme, one tiny `copyright.html` override
to inject the version. Three new Makefile targets (`docs`, `docs-serve`,
`docs-clean`) wrap `uv run zensical …` invocations so the workflow matches
the existing `make build` / `make test` muscle memory.

The site is content-thin on purpose: install + commands reference +
configuration + how-to + architecture. README.md remains the GitHub-facing
quick start and is not touched.

## Technical Details

### Directory layout (new)

```
website/
├── zensical.yml                 # site config (theme, nav, extras)
├── pyproject.toml               # Python project; pins zensical
├── uv.lock                      # committed for reproducible builds
├── README.md                    # uv setup + make targets
├── .gitignore                   # site/, .venv/, __pycache__/
├── overrides/
│   └── partials/
│       └── copyright.html       # footer version injection
└── docs/
    ├── index.md
    ├── install.md
    ├── commands.md
    ├── configuration.md
    ├── how-to.md
    └── architecture.md
```

Build output `website/site/` is gitignored and never committed.

### `zensical.yml` shape

> **Note:** the example below is written from Material/MkDocs muscle memory.
> Several keys (`theme.name`, `theme.custom_dir`, `palette.media/scheme`,
> `features:` items, `!ENV` tag) are not yet confirmed against Zensical's
> current schema. The Task 1 probe validates each before Task 2 writes
> them out; replace unsupported keys with their Zensical equivalents (or
> drop them) when the probe completes. Treat this block as a starting
> sketch, not a contract.

```yaml
site_name: repokit
site_description: Minimal Debian apt repository manager
# site_url: intentionally unset — set when a domain is chosen

repo_url: https://github.com/alkk/repokit
repo_name: alkk/repokit

theme:
  name: zensical            # or whatever the canonical theme key is
  custom_dir: overrides
  palette:
    - media: "(prefers-color-scheme: light)"
      scheme: default
      primary: indigo
      toggle: { icon: material/brightness-7, name: Switch to dark mode }
    - media: "(prefers-color-scheme: dark)"
      scheme: slate
      primary: indigo
      toggle: { icon: material/brightness-4, name: Switch to light mode }
  features:
    - navigation.sections
    - content.code.copy
    - search.suggest

extra:
  version: !ENV REPOKIT_VERSION

nav:
  - Home: index.md
  - Install: install.md
  - Commands: commands.md
  - Configuration: configuration.md
  - How-to: how-to.md
  - Architecture: architecture.md
```

Exact theme key, feature flags, and `!ENV` syntax to be reconciled with
Zensical's current docs at implementation time (Zensical is young; some
Material-derived knobs may not exist yet — fall back to defaults rather
than chasing parity).

### Version injection

`Makefile` already exports `$(REV)`. The new `docs` target:

```make
docs:
	cd website && REPOKIT_VERSION=$(REV) uv run zensical build

docs-serve:
	cd website && REPOKIT_VERSION=$(REV) uv run zensical serve

docs-clean:
	rm -rf website/site
```

`overrides/partials/copyright.html` overrides Zensical's default footer
partial (path mirrors Material's `partials/copyright.html` — verify at
implementation time). Contents render `{{ config.extra.version }}` plus
the year/copyright line.

If `REPOKIT_VERSION` is unset (e.g. building outside Make), Zensical's
`!ENV` should default to empty — the override template handles that case
gracefully (omits the version line rather than rendering `vNone`).

### `pyproject.toml`

```toml
[project]
name = "repokit-docs"
version = "0.0.0"
requires-python = ">=3.11"
dependencies = [
  "zensical>=<pin>",
]
```

Exact lower bound chosen at implementation time from the latest Zensical
release on PyPI. `uv.lock` is committed alongside.

## What Goes Where

- **Implementation Steps**: everything in this plan is in-tree work — config,
  markdown content, theme overrides, Makefile targets.
- **Post-Completion**: choosing a domain and configuring `site_url`; the
  actual hosting/serving setup (nginx config, TLS, etc.); pinning Zensical
  to a newer release when one ships.

## Implementation Steps

### Task 1: Bootstrap the `website/` Python project + Zensical probe

**Files:**
- Create: `website/pyproject.toml`
- Create: `website/.gitignore`
- Modify: `.gitignore` (root)

**Probe (mandatory before Task 2 writes any config):** Zensical is young and
this plan currently asserts several Material-derived conventions (theme key
name, `theme.custom_dir`, `palette`/`media`/`scheme` shape, `features:` list,
`!ENV` YAML tag, override partial path, search/repo-link being default-on).
None of those are confirmed against Zensical's actual surface. The probe
catches drift before it propagates.

- [ ] create `website/pyproject.toml` with project metadata, `requires-python = ">=3.11"`, and a single `zensical` dependency pinned to the latest stable release on PyPI (look up at implementation time)
- [ ] run `cd website && uv sync` to materialise `.venv/` and produce `uv.lock`; commit `uv.lock`
- [ ] create `website/.gitignore` ignoring `site/`, `.venv/`, `__pycache__/`, `.zensical-cache/` (or whatever Zensical's cache dir is named — confirm during probe), `.ruff_cache/`, `.mypy_cache/`; decide on `.python-version` (commit it if `uv sync` produced one, otherwise ignore)
- [ ] add `website/site/` to the root `.gitignore`
- [ ] verify `uv run zensical --version` resolves and prints a version
- [ ] **probe** Zensical's installed package at `website/.venv/lib/python*/site-packages/zensical/` to confirm: (a) exact theme key for `zensical.yml`, (b) whether `theme.custom_dir` is supported, (c) whether the `!ENV` YAML tag is registered for env-var injection, (d) the path/name of the footer/copyright partial available for override (if any), (e) whether built-in search is on by default or needs a plugin entry, (f) whether the GitHub repo-link icon needs explicit feature opt-in
- [ ] **record probe findings** as a short `# Probe notes` block at the top of `website/README.md` (these notes inform Tasks 2 and 3 and stay in the README until removed during a future cleanup); if any assumption fails, note the replacement here
- [ ] verify `git status` shows no accidentally-tracked virtualenv or cache files

### Task 2: Minimal `zensical.yml` + empty content tree (build green)

**Files:**
- Create: `website/zensical.yml`
- Create: `website/docs/index.md` (stub)
- Create: `website/docs/install.md` (stub)
- Create: `website/docs/commands.md` (stub)
- Create: `website/docs/configuration.md` (stub)
- Create: `website/docs/how-to.md` (stub)
- Create: `website/docs/architecture.md` (stub)

- [ ] create `website/zensical.yml` with `site_name`, `site_description`, `repo_url`, `repo_name`, theme palette (indigo, light+dark toggle), built-in search, and the six-item flat `nav` block from Technical Details
- [ ] leave `site_url` commented out with a note explaining when to fill it in
- [ ] create each of the six markdown files with just an H1 placeholder (`# Home`, `# Install`, …) so the nav resolves and the build is green
- [ ] run `uv run zensical build` from `website/` — must succeed with zero broken-link warnings; navigate `site/index.html` opened directly in a browser to confirm relative links work
- [ ] run `uv run zensical serve` and spot-check: dark mode toggle works, search box appears, GitHub header link points to `github.com/alkk/repokit`, all six nav items render and are clickable
- [ ] **verification**: clean `zensical build` + six pages visible + GitHub icon links correctly

### Task 3: Footer version override (with fallback)

**Files:**
- Create: `website/overrides/partials/copyright.html` *(only if Zensical exposes an overridable copyright partial — see fallback)*
- Modify: `website/zensical.yml`

This task depends on the Task 1 probe findings. Three possible paths:

**Path A (preferred): Zensical exposes an overridable copyright partial.**

- [ ] create `website/overrides/partials/copyright.html` (or whatever path the probe identified) rendering the original footer content plus `{% if config.extra.version %}<span class="md-footer-version">{{ config.extra.version }}</span>{% endif %}`
- [ ] add `theme.custom_dir: overrides` to `zensical.yml` (or its Zensical-equivalent)
- [ ] add `extra: { version: !ENV REPOKIT_VERSION }` to `zensical.yml` (only if probe confirmed `!ENV` support; otherwise see Path C)

**Path B (fallback if no overridable partial):** inject the version via
`extra_javascript` or `extra_css` — a tiny script that appends a `<span>` to
the rendered footer. Less clean, still works. Document the choice in
`website/README.md`'s probe notes.

- [ ] add `website/overrides/assets/version.js` (or equivalent) that reads `window.REPOKIT_VERSION` and appends it to the footer
- [ ] template the version into the page via an `extra_*` injection at build time

**Path C (fallback if no `!ENV` tag):** generate `zensical.yml` from a
template at build time. Add `website/zensical.yml.tmpl` and have `make docs`
run `envsubst < zensical.yml.tmpl > zensical.yml` before invoking
`zensical build`. Add the generated `zensical.yml` to `.gitignore`.

**Path D (escape hatch):** if neither override nor env injection works in
the current Zensical release, **drop the footer version entirely** and
document the deferral in Post-Completion. Do not block the plan on this
feature — the rest of the site is independently valuable.

Common to all paths:

- [ ] build with `REPOKIT_VERSION=test-v0.1.0 uv run zensical build` — verify the version surfaces somewhere visible (footer in A/B, or generated config in C)
- [ ] build with `REPOKIT_VERSION` unset — verify nothing renders as `vNone` / empty / broken
- [ ] **verification**: version visible when env set, clean fallback when unset, or the feature is consciously deferred with a Post-Completion note

### Task 4: Author Home + Install pages

**Files:**
- Modify: `website/docs/index.md`
- Modify: `website/docs/install.md`

- [ ] write `index.md`: hero line "Minimal Debian apt repository manager"; three bullets (hashes-once vs freight, atomic dist swaps via `.new→live→.old`, multi-distro with isolated per-(distro,suite) pools — no cross-suite dedup); two CTAs as inline links to Install and How-to; a small fenced code block showing the canonical `init → add → publish` flow (3-4 lines, mirrors the README's quick start spirit but rewritten — do not copy verbatim)
- [ ] write `install.md`: build-from-source (`make build`), binary placement on `$PATH`, `gpg` runtime dep only when signing, gpg-agent provisioning (`gpg --list-secret-keys` check, `repokit` never prompts); state-dir resolution rules (`--repo > $REPOKIT_ROOT > ~/.repokit/`); public-dir resolution (`--public / $REPOKIT_PUBLIC > config.yaml`); one short paragraph on why state/public are split (no leak of `repo.db` under the webserver docroot)
- [ ] `uv run zensical build` clean
- [ ] visual spot check via `uv run zensical serve`: code blocks render with copy buttons, internal links resolve, no broken refs
- [ ] **verification**: both pages render correctly, no build warnings

### Task 5: Author Commands + Configuration pages

**Files:**
- Modify: `website/docs/commands.md`
- Modify: `website/docs/configuration.md`

- [ ] write `commands.md`: one H2 per subcommand in this order — `init`, `add`, `delete`, `list`, `search`, `publish`, `check`, `gc`, `info`, `completion`. For each: one-sentence purpose, bulleted key flags, single example invocation in a fenced code block. Distill from `docs/usage.md` and the actual `--help` output; do not paste verbatim.
- [ ] write `configuration.md`: annotated `config.yaml` example as the lead artifact; sections covering (1) `distros` list semantics including namespaced distros (`customer/acme`) and the plain-prefix conflict rule (`[customer, customer/acme]` is rejected at load); (2) `public_dir` field and its precedence vs. `--public` / `$REPOKIT_PUBLIC`; (3) `suites` map with `<distro>/<suite>` keys and the last-slash split rule for namespaced distros (`customer/acme/stable` → distro `customer/acme`, suite `stable`)
- [ ] `uv run zensical build` clean
- [ ] visual spot check: anchor links within `commands.md` work (table of contents on the right), code blocks correct, all examples valid syntax
- [ ] **verification**: both pages complete and build clean

### Task 6: Author How-to + Architecture pages

**Files:**
- Modify: `website/docs/how-to.md`
- Modify: `website/docs/architecture.md`

- [ ] write `how-to.md`: four task-oriented sections — (1) "Publish a new package end-to-end" (`init → add → publish`, with signing notes), (2) "Clean up orphan pool files" (`gc`, why split from `publish`), (3) "Resolve DB drift after manual file removal" (plain `check` for preview, then `check --purge-db`; explain missing-only-by-design and the FS-glitch reasoning), (4) "Add a customer-namespaced distro" (config edit + `add --distro customer/acme/...` example)
- [ ] write `architecture.md`: state-dir vs. public-dir split and motivation; why `delete` is DB-only (delete→publish window, no apt 404s on retired packages); the `gc` / `check --purge-db` symmetry (file-without-row vs. row-without-file) and the writer-lock-spanning-the-scan invariant; one ASCII diagram of the on-disk layout (`<publicDir>/<distro>/pool/...`, `<publicDir>/<distro>/dists/<suite>/...`, state dir holding `repo.db` and `config.yaml`)
- [ ] `uv run zensical build` clean — cross-links from how-to into commands.md and configuration.md resolve
- [ ] visual spot check: ASCII diagram renders correctly in both light and dark themes (monospace, no wrapping), section anchors work
- [ ] **verification**: both pages complete, all internal cross-links resolve, build clean

### Task 7: Makefile docs targets + `website/README.md`

**Files:**
- Modify: `Makefile`
- Create: `website/README.md`

Authoring `website/README.md` is deferred from Task 1 to here so its
documented commands (`make docs`, `make docs-serve`, `make docs-clean`)
exist at the moment the README claims they do.

- [ ] add `docs` target: `cd website && REPOKIT_VERSION=$(REV) uv run zensical build --strict` (use `--strict` or Zensical's documented equivalent to fail the build on broken-link warnings; if no equivalent exists, drop the flag and document why in the probe notes)
- [ ] add `docs-serve` target: `cd website && REPOKIT_VERSION=$(REV) uv run zensical serve` (note: blocks on the dev server; `Ctrl-C` to stop)
- [ ] add `docs-clean` target: `rm -rf website/site`
- [ ] mark all three as `.PHONY`
- [ ] write `website/README.md` covering: prerequisites (Python ≥ 3.11, `uv` installed), `uv sync` once, and the three `make docs*` targets — plus the probe notes section from Task 1 (these stay until a future cleanup)
- [ ] from a fresh checkout, run `make docs` — verify it builds the site with the version embedded (or consciously deferred per Task 3 outcome)
- [ ] run `make docs-clean` — verify `website/site/` is gone
- [ ] run `make docs-serve` briefly — verify the dev server starts and `http://127.0.0.1:8000/` serves the index
- [ ] **verification**: all three targets work; `--strict` (or equivalent) actually fails the build on an introduced broken link (try it briefly by adding a `[broken](missing.md)` link, build, then revert); existing `make build` / `make test` / `make lint` still pass unchanged

### Task 8: Verify acceptance criteria

- [ ] all six pages render and are reachable from the top nav
- [ ] `make docs` produces `website/site/` with the footer version baked in (or consciously deferred per Task 3 Path D — note in the plan if so)
- [ ] `make docs-serve` runs the dev server and live-reloads on markdown edits
- [ ] `make docs-clean` removes the output cleanly
- [ ] dark mode toggle works and persists across page navigation
- [ ] search returns hits for terms like "publish", "purge-db", "namespaced" (assuming Task 1 probe confirmed search is available; if not, this becomes a Post-Completion item)
- [ ] GitHub header icon links to `https://github.com/alkk/repokit`
- [ ] `make docs` exits non-zero on an injected broken link (verifies `--strict` or equivalent is wired)
- [ ] `make test` and `make lint` still pass (sanity — this plan touches no Go)
- [ ] root `.gitignore` and `website/.gitignore` together exclude `site/`, `.venv/`, `uv` caches, `__pycache__/`, `.python-version` if not committed

### Task 9: Move plan to completed

- [ ] `mkdir -p docs/plans/completed`
- [ ] `git mv docs/plans/20260519-zensical-docs-website.md docs/plans/completed/`
- [ ] commit with message in the project's existing convention

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes,
informational only.*

**Domain & `site_url`:**
- Pick the hostname where the docs will live (planned: dedicated subdomain
  serving the site at root).
- Edit `website/zensical.yml` to uncomment and set `site_url:
  https://<chosen-host>/`. This is what makes canonical URLs and OpenGraph
  tags correct; relative links inside the site already work without it.

**Hosting setup (out of scope for this plan):**
- Decide on the webserver and TLS source (nginx + Let's Encrypt is the
  obvious match, given the project's apt-server context).
- Wire a transfer/sync mechanism from a developer machine (or a CI runner,
  later) to the serving location. Manual `rsync -av website/site/
  user@host:/var/www/repokit-docs/` is fine for now.

**Zensical version pin:**
- Zensical is young and likely to release breaking changes pre-1.0. Re-pin
  `pyproject.toml` periodically; re-run `uv sync` and rebuild to catch any
  template / config changes that affect the override or theme palette.

**README cross-link (optional, after a domain is chosen):**
- Add a single line to the top of `README.md` pointing to the docs site
  (e.g. "Full documentation: https://<host>"). Deliberately deferred until
  the URL is real.
