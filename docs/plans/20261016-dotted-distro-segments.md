# Allow dots in distro name segments

## Overview

Make versioned repos like `deb http://packages.netxms.org/7.x/debian trixie main`
possible by letting distro segments contain `.`, so `7.x/debian` is a valid
one-level namespaced distro alongside the existing `debian` and `devel/debian`.
Today `validateDistroSegment` only allows `[a-zA-Z0-9-]`, so `7.x` is rejected.

## Decisions

- **The version is just a namespace.** `7.x/debian` is an ordinary namespaced
  distro. repokit gains no notion of a "release". Ruled out:
  - a first-class release dimension (DB column, `--release` flag, a `releases:`
    config axis crossed with `distros:`): it gives the same on-disk layout at
    many times the cost, and nothing needs version-aware queries;
  - the no-code workaround `v7/debian`: the user wants the `7.x` URL.
- **Devel repos stay unversioned** (`devel/debian`). The depth limit of one
  `/` stays, so `7.x/devel/debian` remains rejected on purpose.
- **New segment rule:** the alphabet becomes `[a-zA-Z0-9.-]`.
  - No leading `-` (unchanged).
  - No leading `.` (new): keeps hidden directories (`.git`, `.well-known`)
    out of the docroot. This also covers `.` and `..`, so the separate
    dot-segment check can go unless its error message is clearer.
  - No trailing `.` (new): nothing needs `7.`, and Windows (which the binary
    still compiles for, although goreleaser ships only linux/darwin) strips
    trailing dots, so `7.` would alias `7`.
  - No `..` anywhere in a segment (new). This is grammar policy, not
    traversal protection: `hasParentSegment` already allows `foo..bar` and
    rejects only real `..` path segments. The rule is there because nothing
    needs `7..x`, and a stricter grammar is easier to relax later than to
    tighten. It is stricter than `validateScopeName`, which allows interior
    and trailing dots in suites; the difference is deliberate.
- **Seeding and promotion need no new command.** `add` accepts any `.deb`
  path, including another distro's pool file. It builds a fresh row and
  pool path for the target distro, and the DB uniqueness key includes the
  full distro string. So:
  - seeding 7.x from the current repo is
    `repokit add --distro 7.x/debian --suite trixie --component main
    /u01/packages/debian/pool/trixie/main/n/<pkg>/<pkg>_7.*.deb`, then
    `repokit publish`;
  - promoting from devel is the same command with a `devel/debian` pool
    path.

  The cost: bytes and hashes are duplicated per distro, there's no
  provenance record of where a package was promoted from, and there's no
  atomic multi-suite or cross-OS promotion and no whole-repo snapshot. If
  that ever hurts, a generic `copy`/`promote` command between distros can
  address it without introducing a release axis.
- **No version guard.** Nothing stops a 6.x `.deb` from landing in
  `7.x/debian`. If that is ever wanted, it becomes an optional per-distro
  setting later.

## Discovery facts

- Every entry point already goes through `config.ValidateDistroName`:
  - `distros:` at config load;
  - the distro half of `suites:` keys (`splitSuiteKey` cuts on the last `/`,
    so `7.x/debian/trixie` yields `7.x/debian` + `trixie`);
  - `--distro` via `validateDistroScope`;
  - publish's `dbScopes` discovery.

  Only the segment validator changes. There are no schema, pool, publish or
  gc/check changes. (`list` and `search` don't validate `--distro`; it is
  only an exact-match SQL filter there, which is unaffected.)
- Other code that handles distro names is also safe with dots:
  - gc's orphan matching (`matchOrphanDistro`) matches on `<distro>/pool/`;
    the trailing `/` keeps `7` and `7.x` from shadowing each other;
  - the `.tmp`/`.bak` skips apply only to pool file names, not directories.
- YAML: unquoted `- 7.10` in `distros:` decodes into the string `"7.10"`
  (not the float `7.1`), and so does a `7.10/...` key in `suites:`
  (checked against the vendored yaml.v3). No quoting is needed in
  `config.yaml`.
- The `.new`/`.old` staging suffixes apply only to suite directories, never to
  distro directories, so a dotted distro cannot collide with staging.
- If `Origin`/`Label` are not set in `suites:`, they default to the full
  distro name. So `7.x/debian` appears verbatim in the Release `Origin`,
  `Label` and `Description` headers. A dot is safe there, because apt pin
  syntax splits on `,` and `=`.
- `init_test.go`'s seed-distro regex (`^[a-zA-Z0-9-]+$`) still holds for the
  seeded names. Leave it unchanged.

## Development Approach

TDD: add the failing cases first, then relax the validator.

## Implementation Steps

### Task 1: Accept dotted distro segments

Layers: config validation (`pkg/config`), CLI integration test, user docs.

- [ ] extend `TestValidateDistroName`:
  - accept: `7.x`, `7.x/debian`, `devel/7.0`, `7.10`
  - reject: `.x`, `7.`, `.hidden/debian`, `7.x/.`, `7.x/..`, `debian/7.`,
    `7..x`, `7.x/de..bian`
- [ ] add one accept case (`7.x/debian`) to `TestValidateDistroScope` (the
  CLI-level mirror)
- [ ] add `TestLoad` subtests:
  - `distros: [7.x/debian, debian]` loads, and a `suites:` override keyed
    `7.x/debian/trixie` resolves to that distro/suite;
  - an unquoted plain `7.10` distro stays `"7.10"`.
- [ ] parametrize `TestIntegration_NamespacedDistro` over
  `{customer/acme, 7.x/debian}`, building paths with `filepath.FromSlash`
  and keeping its negative case once. For `7.x/debian` it should show that
  the pool file lands under `<public>/7.x/debian/pool/`, that
  `dists/<suite>/InRelease` is produced, and that the default
  `Origin`/`Label` are `7.x/debian`.
- [ ] fix the test's filesystem-discovery check. Its current second
  unscoped publish does not isolate `filesystemScopes`, because the DB row
  still exists and `allScopes` unions both sources. Replace it with:
  1. `delete` the only package;
  2. run an unscoped `publish`. An empty suite gets no index files at all,
     so assert:
     - the previously existing `Packages` file is gone;
     - the live `Release` still exists, with empty `Components` and
       `Architectures` and no checksum entries for the old indexes.

     If `filesystemScopes` dropped the distro, the old non-empty
     `Packages` would still be there, so its absence proves discovery;
  3. run `gc` and assert the orphaned pool file under the namespaced pool
     is reaped.

  This fixes a gap that already exists for `customer/acme` as well.
- [ ] confirm the new cases fail, then relax `validateDistroSegment` to the
  rule in Decisions
- [ ] update the doc comments on `ValidateDistroName` and
  `validateDistroSegment` to the new rule
- [ ] update `docs/usage.md` (the `--distro` validation paragraph and the
  `distros` row of the config table). It should state the widened alphabet
  plus the no-leading-`.`, no-trailing-`.` and no-`..` rules.
- [ ] gate: `make test` and `make lint` pass
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

Production rollout on packages.netxms.org (operator steps, no code):
1. Add `7.x/debian` and `7.x/ubuntu` to `distros:` in `config.yaml`.
2. Optionally add `suites:` overrides (e.g. `7.x/debian/trixie`) with
   `label: NetXMS 7.x` so the Release `Label` is not the raw distro name.
3. Run `repokit add --distro 7.x/debian …` for the 7.x packages, then
   `repokit publish`.
4. Verify on a client:
   `deb http://packages.netxms.org/7.x/debian trixie main`, then `apt update`.

The existing `debian`/`ubuntu`/`devel/*` repos are untouched.
