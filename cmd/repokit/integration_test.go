package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blakesmith/ar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_InitAddPublish exercises the full happy path: init the repo,
// stage one fixture .deb, then publish --no-sign. Asserts that:
//   - the pool file is in place under the public dir
//   - dists/.../Packages contains the expected single stanza
//   - Release references all three Packages variants
//   - for every Packages/Packages.gz/Packages.xz file, the size and four hashes
//     recorded in Release match the bytes on disk byte-for-byte (this is the
//     bug-magnet case for control-stanza-newline drift)
//   - the state dir contains exactly {config.yaml, repo.db} (no public-tree leak)
//
// This case exercises the --public flag path explicitly.
func TestIntegration_InitAddPublish(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	runCmd := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		full := append([]string{"--repo", tmpState, "--public", tmpPublic, name}, args...)
		code := run(full, &stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "%s failed: %s", name, stderr.String())
	}

	// init does not consume --public (and would not need it), but run accepts
	// the flag at the global level regardless, so we can keep one runCmd helper.
	runCmd("init")
	appendTestDistros(t, tmpState, "debian", "ubuntu")
	runCmd("add", "--distro", "debian", "--suite", "bookworm", deb)
	runCmd("publish", "--distro", "debian", "--suite", "bookworm", "--no-sign")

	suiteDir := filepath.Join(tmpPublic, "debian", "dists", "bookworm")
	binDir := filepath.Join(suiteDir, "main", "binary-amd64")

	poolFile := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main",
		"h", "hello-gz", "hello-gz_1.0_amd64.deb")
	assert.FileExists(t, poolFile)

	assertStateDirIsPrivate(t, tmpState)

	pkgPath := filepath.Join(binDir, "Packages")
	pkgBody, err := os.ReadFile(pkgPath)
	require.NoError(t, err)
	stanzas := splitPackagesStanzas(pkgBody)
	require.Len(t, stanzas, 1, "exactly one stanza expected")
	assert.Contains(t, stanzas[0], "Package: hello-gz\n")
	assert.Contains(t, stanzas[0], "Filename: pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb\n")

	releaseBody, err := os.ReadFile(filepath.Join(suiteDir, "Release"))
	require.NoError(t, err)
	releaseText := string(releaseBody)

	for _, base := range []string{"Packages", "Packages.gz", "Packages.xz"} {
		assert.Containsf(t, releaseText, "main/binary-amd64/"+base,
			"Release should reference %s", base)
	}
	assert.FileExists(t, filepath.Join(suiteDir, "InRelease"))
	assert.FileExists(t, filepath.Join(suiteDir, "Release.gpg"))

	for _, base := range []string{"Packages", "Packages.gz", "Packages.xz"} {
		relPath := "main/binary-amd64/" + base
		body, err := os.ReadFile(filepath.Join(binDir, base))
		require.NoErrorf(t, err, "read %s", relPath)

		entries := releaseEntriesFor(t, releaseText, relPath)
		require.Lenf(t, entries, 4, "expected 4 hash blocks for %s", relPath)

		assert.Equalf(t, int64(len(body)), entries["MD5Sum"].size,
			"size mismatch for %s in MD5Sum block", relPath)
		assert.Equalf(t, hashHex(md5.New(), body), entries["MD5Sum"].hash,
			"MD5 mismatch for %s", relPath)
		assert.Equalf(t, hashHex(sha1.New(), body), entries["SHA1"].hash,
			"SHA1 mismatch for %s", relPath)
		assert.Equalf(t, hashHex(sha256.New(), body), entries["SHA256"].hash,
			"SHA256 mismatch for %s", relPath)
		assert.Equalf(t, hashHex(sha512.New(), body), entries["SHA512"].hash,
			"SHA512 mismatch for %s", relPath)
	}
}

// TestIntegration_MultiSuiteMultiArchMultiComponent exercises the full layout:
// 2 distros × 2 suites × 2 components × {amd64, arm64, all}. It asserts the
// no-cross-suite-dedup invariant, that arch=all rows fan out into every
// concrete binary-<arch>/Packages with no binary-all/ directory created, and
// that Architectures: in Release excludes 'all'. Then it deletes one version
// and re-publishes to confirm scoped removal leaves siblings intact.
//
// This case exercises the REPOKIT_PUBLIC env-var path (no --public flag).
func TestIntegration_MultiSuiteMultiArchMultiComponent(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	debDir := t.TempDir()
	t.Setenv(envPublic, tmpPublic)

	runCmd := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--repo", tmpState, name}, args...), &stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "%s failed: %s", name, stderr.String())
	}

	runCmd("init")
	appendTestDistros(t, tmpState, "debian", "ubuntu")

	distros := []string{"debian", "ubuntu"}
	suitesByDistro := map[string][]string{
		"debian": {"bookworm", "bullseye"},
		"ubuntu": {"jammy", "noble"},
	}
	components := []string{"main", "contrib"}
	concreteArches := []string{"amd64", "arm64"}
	matrixArches := []string{"amd64", "arm64", "all"}
	const (
		pkgName = "alpha"
		pkgVer  = "1.0"
	)

	debCache := map[string]string{}
	debFor := func(name, version, arch string) string {
		key := name + "_" + version + "_" + arch
		if p, ok := debCache[key]; ok {
			return p
		}
		p := buildTestDeb(t, debDir, name, version, arch)
		debCache[key] = p
		return p
	}

	for _, distro := range distros {
		for _, suite := range suitesByDistro[distro] {
			for _, comp := range components {
				args := make([]string, 0, 7+len(matrixArches))
				args = append(args, "add",
					"--distro", distro,
					"--suite", suite,
					"--component", comp)
				for _, arch := range matrixArches {
					args = append(args, debFor(pkgName, pkgVer, arch))
				}
				runCmd(args[0], args[1:]...)
			}
		}
	}

	// add a second version into a single scope so the later delete can prove
	// "other versions remain" rather than just "other suites remain".
	extraDeb := debFor(pkgName, "2.0", "amd64")
	runCmd("add",
		"--distro", "debian", "--suite", "bookworm", "--component", "main",
		extraDeb)

	runCmd("publish", "--no-sign")

	for _, distro := range distros {
		for _, suite := range suitesByDistro[distro] {
			assertSuiteLayout(t, tmpPublic, distro, suite, components, concreteArches)
		}
	}

	// extra version landed only in debian/bookworm/main/binary-amd64.
	bookwormAmd64 := filepath.Join(tmpPublic, "debian", "dists", "bookworm",
		"main", "binary-amd64", "Packages")
	stanzas := readStanzas(t, bookwormAmd64)
	require.Len(t, stanzas, 3, "expected 1.0/amd64 + 2.0/amd64 + 1.0/all")
	assert.True(t, hasStanza(stanzas, "alpha", "2.0", "amd64"),
		"alpha 2.0 amd64 should be present before delete")

	// no-cross-suite-dedup: same NVRA in two suites of the same distro must
	// produce two distinct physical pool files with identical content.
	assertNoCrossSuiteDedup(t, tmpPublic)

	// scoped delete + republish: drop alpha 1.0 amd64 from debian/bookworm/main
	// only. Other versions in the same scope and other scopes must survive.
	runCmd("delete",
		"--distro", "debian", "--suite", "bookworm", "--component", "main",
		"--arch", "amd64", "--name", pkgName, "--version", pkgVer)
	runCmd("publish", "--distro", "debian", "--suite", "bookworm", "--no-sign")

	stanzasAfter := readStanzas(t, bookwormAmd64)
	require.Len(t, stanzasAfter, 2, "1.0/amd64 should be gone, 2.0/amd64 + 1.0/all remain")
	assert.False(t, hasStanza(stanzasAfter, pkgName, pkgVer, "amd64"),
		"alpha 1.0 amd64 should be gone from binary-amd64/Packages")
	assert.True(t, hasStanza(stanzasAfter, pkgName, "2.0", "amd64"),
		"alpha 2.0 amd64 should remain")
	assert.True(t, hasStanza(stanzasAfter, pkgName, pkgVer, "all"),
		"alpha 1.0 arch=all should remain (different row, untouched by delete)")

	// debian/bookworm/main/binary-arm64 still has its 1.0 amd64-equivalent
	// row (i.e., 1.0/arm64) and the 1.0/all row — delete was scoped to arch=amd64.
	bookwormArm64 := filepath.Join(tmpPublic, "debian", "dists", "bookworm",
		"main", "binary-arm64", "Packages")
	armStanzas := readStanzas(t, bookwormArm64)
	require.Len(t, armStanzas, 2)
	assert.True(t, hasStanza(armStanzas, pkgName, pkgVer, "arm64"))
	assert.True(t, hasStanza(armStanzas, pkgName, pkgVer, "all"))

	// debian/bullseye/main/binary-amd64 untouched.
	bullseyeAmd64 := filepath.Join(tmpPublic, "debian", "dists", "bullseye",
		"main", "binary-amd64", "Packages")
	bsStanzas := readStanzas(t, bullseyeAmd64)
	require.Len(t, bsStanzas, 2)
	assert.True(t, hasStanza(bsStanzas, pkgName, pkgVer, "amd64"))
	assert.True(t, hasStanza(bsStanzas, pkgName, pkgVer, "all"))

	// pool file for the deleted row is gone; siblings remain.
	deletedPool := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main",
		"a", "alpha", "alpha_1.0_amd64.deb")
	_, err := os.Stat(deletedPool)
	require.ErrorIsf(t, err, fs.ErrNotExist, "deleted pool file should be gone (got err=%v)", err)
	survivor := filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main",
		"a", "alpha", "alpha_1.0_amd64.deb")
	assert.FileExists(t, survivor)

	assertStateDirIsPrivate(t, tmpState)
}

// TestIntegration_CrossDistroSameSuite confirms that two distros publishing the
// same suite name (e.g. debian/trixie + ubuntu/trixie) end up with two
// independent <distro>/dists/<suite> trees, each with its own Release/Packages
// and its own pool tree. There is no shared dists/ or pool/ namespace at the
// public root that would cause one distro's publish to clobber the other.
//
// This case exercises the public_dir-via-config.yaml resolution path: after
// init, the REPLACE_ME placeholder is rewritten to tmpPublic and subsequent
// commands run with neither --public nor REPOKIT_PUBLIC set.
func TestIntegration_CrossDistroSameSuite(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	debDir := t.TempDir()

	runCmd := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--repo", tmpState, name}, args...), &stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "%s failed: %s", name, stderr.String())
	}

	runCmd("init")
	appendTestDistros(t, tmpState, "debian", "ubuntu")
	rewriteConfigPublicDir(t, tmpState, tmpPublic)

	debianDeb := buildTestDeb(t, debDir, "alpha", "1.0", "amd64")
	ubuntuDeb := buildTestDeb(t, debDir, "beta", "2.0", "amd64")

	runCmd("add", "--distro", "debian", "--suite", "trixie", debianDeb)
	runCmd("add", "--distro", "ubuntu", "--suite", "trixie", ubuntuDeb)
	runCmd("publish", "--no-sign")

	debianSuite := filepath.Join(tmpPublic, "debian", "dists", "trixie")
	ubuntuSuite := filepath.Join(tmpPublic, "ubuntu", "dists", "trixie")
	assert.DirExists(t, debianSuite)
	assert.DirExists(t, ubuntuSuite)
	assert.FileExists(t, filepath.Join(debianSuite, "Release"))
	assert.FileExists(t, filepath.Join(ubuntuSuite, "Release"))

	debianStanzas := readStanzas(t, filepath.Join(debianSuite,
		"main", "binary-amd64", "Packages"))
	require.Len(t, debianStanzas, 1, "debian/trixie should have only its own package")
	assert.True(t, hasStanza(debianStanzas, "alpha", "1.0", "amd64"))
	assert.False(t, hasStanza(debianStanzas, "beta", "2.0", "amd64"),
		"debian/trixie must not see ubuntu/trixie packages")

	ubuntuStanzas := readStanzas(t, filepath.Join(ubuntuSuite,
		"main", "binary-amd64", "Packages"))
	require.Len(t, ubuntuStanzas, 1, "ubuntu/trixie should have only its own package")
	assert.True(t, hasStanza(ubuntuStanzas, "beta", "2.0", "amd64"))
	assert.False(t, hasStanza(ubuntuStanzas, "alpha", "1.0", "amd64"),
		"ubuntu/trixie must not see debian/trixie packages")

	assert.FileExists(t, filepath.Join(tmpPublic, "debian", "pool", "trixie",
		"main", "a", "alpha", "alpha_1.0_amd64.deb"))
	assert.FileExists(t, filepath.Join(tmpPublic, "ubuntu", "pool", "trixie",
		"main", "b", "beta", "beta_2.0_amd64.deb"))

	// no shared top-level dists/ or pool/ should be created at the public root
	// either — those would indicate a regression to the old shared-namespace layout.
	_, err := os.Stat(filepath.Join(tmpPublic, "dists"))
	require.ErrorIsf(t, err, fs.ErrNotExist, "no shared <publicDir>/dists/ should exist")
	_, err = os.Stat(filepath.Join(tmpPublic, "pool"))
	require.ErrorIsf(t, err, fs.ErrNotExist, "no shared <publicDir>/pool/ should exist")

	assertStateDirIsPrivate(t, tmpState)
}

// TestIntegration_NamespacedDistro exercises the full namespaced-distro flow
// end-to-end: init with a single `customer/acme` distro, add one .deb, then run
// an UNSCOPED publish so the dbScopes path (publish.go:515) is what discovers
// the namespaced scope. Without the validateDistroScope repointing from Task 3,
// dbScopes would silently filter the customer/acme row and publish would emit
// nothing — so the unscoped variant is the only one that exercises that fix.
//
// Asserts the on-disk layout, all five Release header defaults from
// defaultSuite (Origin/Label/Suite/Codename/Description), apt-relative
// Filename in Packages, and that verify exits 0. Also asserts the negative
// case where add against an unconfigured namespaced distro is rejected by
// requireConfiguredDistro (the scope validator accepts foo/bar but it is
// absent from config.distros).
func TestIntegration_NamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	runCmd := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		full := append([]string{"--repo", tmpState, "--public", tmpPublic, name}, args...)
		code := run(full, &stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "%s failed: %s", name, stderr.String())
	}

	runCmd("init")
	appendTestDistros(t, tmpState, "customer/acme")
	runCmd("add", "--distro", "customer/acme", "--suite", "stable", deb)
	// Unscoped publish: dbScopes is the discovery path. With Task 3's
	// validateDistroScope swap the namespaced row surfaces; without it,
	// publish would emit zero suites.
	runCmd("publish", "--no-sign")

	poolFile := filepath.Join(tmpPublic, "customer", "acme", "pool", "stable",
		"main", "h", "hello-gz", "hello-gz_1.0_amd64.deb")
	assert.FileExists(t, poolFile,
		"pool file must land under the namespaced subtree")

	suiteDir := filepath.Join(tmpPublic, "customer", "acme", "dists", "stable")
	assert.FileExists(t, filepath.Join(suiteDir, "Release"))
	assert.FileExists(t, filepath.Join(suiteDir, "InRelease"))
	assert.FileExists(t, filepath.Join(suiteDir, "Release.gpg"))

	releaseBody, err := os.ReadFile(filepath.Join(suiteDir, "Release"))
	require.NoError(t, err)
	releaseText := string(releaseBody)

	// All five defaultSuite headers must be emitted verbatim — Origin/Label
	// reflect the full slashed distro name (no segment splitting), and the
	// composite Description uses the same verbatim form.
	assert.Equal(t, "customer/acme", headerValue(t, releaseText, "Origin"))
	assert.Equal(t, "customer/acme", headerValue(t, releaseText, "Label"))
	assert.Equal(t, "stable", headerValue(t, releaseText, "Suite"))
	assert.Equal(t, "stable", headerValue(t, releaseText, "Codename"))
	assert.Equal(t, "customer/acme stable", headerValue(t, releaseText, "Description"))

	pkgPath := filepath.Join(suiteDir, "main", "binary-amd64", "Packages")
	pkgBody, err := os.ReadFile(pkgPath)
	require.NoError(t, err)
	pkgText := string(pkgBody)
	// Filename inside Packages is apt-relative: the customer/acme/ segment
	// comes from the apt URI base, not the Filename field. A leading
	// customer/acme/ here would mean clients double-prefix the path.
	assert.Contains(t, pkgText,
		"Filename: pool/stable/main/h/hello-gz/hello-gz_1.0_amd64.deb\n",
		"Filename must be apt-relative (no leading customer/acme/ segment)")
	assert.NotContains(t, pkgText, "Filename: customer/acme/",
		"namespaced segment must come from the URI base, not the Filename field")

	runCmd("verify")

	// Second unscoped publish: now that a live customer/acme/dists/stable/Release
	// exists on disk, the filesystemScopes half of allScopes (publish.go) is the
	// one that has to discover the namespaced subtree — exercise it explicitly.
	runCmd("publish", "--no-sign")
	assert.FileExists(t, filepath.Join(suiteDir, "Release"),
		"re-publish via the filesystem-scope path must keep the namespaced suite")

	assertStateDirIsPrivate(t, tmpState)

	// Negative case: a syntactically valid namespaced distro that is NOT in
	// config.distros must be rejected by requireConfiguredDistro. The scope
	// validator accepts foo/bar, so this proves the configured-distro gate
	// still fires for the namespaced form.
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "foo/bar", "--suite", "stable", deb,
	}, &stdout, &stderr, "test-rev")
	require.NotEqual(t, exitOK, code,
		"add against unconfigured namespaced distro must fail")
	assert.Contains(t, stderr.String(), "not in config.distros",
		"failure must come from requireConfiguredDistro, not the scope validator")
}

// TestIntegration_Completion drives the full TAB-completion flow end-to-end
// through run with GO_FLAGS_COMPLETION=1 set. The two sub-tests cover the
// two completion code paths that ship in this branch:
//
//   - subcommand-name completion: go-flags lists every top-level command when
//     the trailing token is empty.
//   - --distro value completion: go-flags routes through *DistroName.Complete,
//     which reads config.yaml from the resolved state dir and returns every
//     configured distro (plain `debian` and the namespaced `customer/acme`).
//
// REPOKIT_ROOT is set via t.Setenv so completionStateDir() resolves to tmpState
// — the test binary's os.Args doesn't carry --repo, and Options.Repo is not
// populated by go-flags in completion mode. The trailing empty argv element is
// go-flags' "complete here" marker (see vendor/.../go-flags/completion.go:244,
// where lastarg = s.args[len-1] is the partial prefix being completed).
//
// t.Setenv guarantees both GO_FLAGS_COMPLETION and REPOKIT_ROOT are restored at
// test end, so neither leaks into subsequent tests in the package.
func TestIntegration_Completion(t *testing.T) {
	tmpState := t.TempDir()

	var bootStdout, bootStderr bytes.Buffer
	code := run([]string{"--repo", tmpState, "init"}, &bootStdout, &bootStderr, "test-rev")
	require.Equalf(t, exitOK, code, "init failed: %s", bootStderr.String())
	appendTestDistros(t, tmpState, "debian", "customer/acme")

	t.Setenv("GO_FLAGS_COMPLETION", "1")
	t.Setenv(envRepo, tmpState)

	t.Run("subcommand names", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--repo", tmpState, ""}, &stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "stderr: %s", stderr.String())
		assert.Empty(t, stderr.String(), "stderr must stay clean during completion")
		out := stdout.String()
		for _, name := range []string{
			cmdInit, cmdAdd, cmdDelete, cmdList, cmdSearch,
			cmdPublish, cmdVerify, cmdInfo, cmdCompletion,
		} {
			assert.Containsf(t, out, name,
				"subcommand %q missing from completion output:\n%s", name, out)
		}
	})

	t.Run("distro values", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--repo", tmpState, "add", "--distro", ""},
			&stdout, &stderr, "test-rev")
		require.Equalf(t, exitOK, code, "stderr: %s", stderr.String())
		assert.Empty(t, stderr.String(), "stderr must stay clean during completion")
		out := stdout.String()
		assert.Contains(t, out, "debian",
			"plain distro must appear in --distro completion output")
		assert.Contains(t, out, "customer/acme",
			"namespaced distro must round-trip through completion")
	})
}

// assertSuiteLayout verifies the published structure for one (distro, suite):
// concrete binary-<arch> directories exist, binary-all does not, the Packages
// triple is present and consistent with Release, and the Release header lists
// only concrete arches. publicDir is the root of the public-facing tree that
// holds <distro>/dists/ and <distro>/pool/.
func assertSuiteLayout(t *testing.T, publicDir, distro, suite string,
	components, concreteArches []string) {
	t.Helper()
	suiteDir := filepath.Join(publicDir, distro, "dists", suite)

	for _, comp := range components {
		_, err := os.Stat(filepath.Join(suiteDir, comp, "binary-all"))
		require.ErrorIsf(t, err, fs.ErrNotExist,
			"%s/%s/%s/binary-all must not be created", distro, suite, comp)
	}

	for _, comp := range components {
		for _, arch := range concreteArches {
			pkgPath := filepath.Join(suiteDir, comp, "binary-"+arch, "Packages")
			stanzas := readStanzas(t, pkgPath)
			// every scope receives alpha_1.0_<arch> + alpha_1.0_all; the
			// extra alpha_2.0_amd64 case is asserted separately by the caller
			// after this helper returns, so here we expect exactly two.
			wantCount := 2
			if distro == "debian" && suite == "bookworm" && comp == "main" && arch == "amd64" {
				wantCount = 3
			}
			require.Lenf(t, stanzas, wantCount,
				"%s/%s/%s/binary-%s should contain %d stanzas", distro, suite, comp, arch, wantCount)
			assert.True(t, hasStanza(stanzas, "alpha", "1.0", arch),
				"alpha 1.0 %s missing from %s/%s/%s", arch, distro, suite, comp)
			assert.True(t, hasStanza(stanzas, "alpha", "1.0", "all"),
				"arch=all stanza missing from %s/%s/%s/binary-%s", distro, suite, comp, arch)
		}
	}

	releaseBody, err := os.ReadFile(filepath.Join(suiteDir, "Release"))
	require.NoError(t, err)
	releaseText := string(releaseBody)

	archs := strings.Fields(headerValue(t, releaseText, "Architectures"))
	sort.Strings(archs)
	assert.Equal(t, concreteArches, archs,
		"Release Architectures should equal concrete arches (no 'all') for %s/%s",
		distro, suite)
	assert.NotContains(t, archs, "all", "Architectures must exclude 'all'")

	comps := strings.Fields(headerValue(t, releaseText, "Components"))
	sort.Strings(comps)
	wantComps := append([]string(nil), components...)
	sort.Strings(wantComps)
	assert.Equal(t, wantComps, comps, "Release Components mismatch for %s/%s", distro, suite)

	for _, comp := range components {
		for _, arch := range concreteArches {
			for _, base := range []string{"Packages", "Packages.gz", "Packages.xz"} {
				rel := comp + "/binary-" + arch + "/" + base
				body, err := os.ReadFile(filepath.Join(suiteDir, rel))
				require.NoErrorf(t, err, "read %s in %s/%s", rel, distro, suite)
				entries := releaseEntriesFor(t, releaseText, rel)
				require.Lenf(t, entries, 4, "%s missing hash blocks in %s/%s Release",
					rel, distro, suite)
				assert.Equalf(t, int64(len(body)), entries["MD5Sum"].size,
					"size mismatch for %s in %s/%s", rel, distro, suite)
				assert.Equalf(t, hashHex(sha256.New(), body), entries["SHA256"].hash,
					"sha256 mismatch for %s in %s/%s", rel, distro, suite)
			}
		}
	}
}

// assertNoCrossSuiteDedup confirms that the same NVRA staged into two suites
// of the same distro lands at distinct physical pool paths with byte-identical
// content but separate inodes — repokit never aliases pool files across suites.
func assertNoCrossSuiteDedup(t *testing.T, publicDir string) {
	t.Helper()
	bookworm := filepath.Join(publicDir, "debian", "pool", "bookworm", "main",
		"a", "alpha", "alpha_1.0_amd64.deb")
	bullseye := filepath.Join(publicDir, "debian", "pool", "bullseye", "main",
		"a", "alpha", "alpha_1.0_amd64.deb")

	require.NotEqual(t, bookworm, bullseye, "pool paths must differ across suites")

	info1, err := os.Stat(bookworm)
	require.NoError(t, err)
	info2, err := os.Stat(bullseye)
	require.NoError(t, err)
	assert.False(t, os.SameFile(info1, info2),
		"pool files in two suites must be distinct physical files")

	body1, err := os.ReadFile(bookworm)
	require.NoError(t, err)
	body2, err := os.ReadFile(bullseye)
	require.NoError(t, err)
	assert.Equal(t, body1, body2,
		"pool file content must be identical across suites (same source .deb)")
}

// assertStateDirIsPrivate confirms that no public-tree files leaked into the
// state dir: only config.yaml and repo.db (plus its sqlite -journal/-wal/-shm
// siblings, if any) are allowed at the top level. A leaked <distro>/ directory
// would mean some command still treats the state dir as the public root.
func assertStateDirIsPrivate(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		switch name {
		case configFilename, dbFilename:
			continue
		}
		// sqlite may leave behind ephemeral siblings of repo.db (rare for our
		// closed-handle workflow, but cheap to tolerate).
		if strings.HasPrefix(name, dbFilename+"-") {
			continue
		}
		t.Fatalf("state dir leaked unexpected entry %q (isDir=%v); only config.yaml + repo.db allowed",
			name, e.IsDir())
	}
}

// rewriteConfigPublicDir replaces the REPLACE_ME placeholder in the freshly-
// init'd <stateDir>/config.yaml with publicDir, so subsequent commands resolve
// public_dir through the config path with neither --public nor REPOKIT_PUBLIC set.
func rewriteConfigPublicDir(t *testing.T, stateDir, publicDir string) {
	t.Helper()
	cfgPath := filepath.Join(stateDir, configFilename)
	body, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	updated := strings.Replace(string(body),
		`public_dir: "REPLACE_ME"`,
		`public_dir: "`+publicDir+`"`,
		1)
	require.NotEqual(t, string(body), updated, "config template must contain a public_dir REPLACE_ME line")
	require.NoError(t, os.WriteFile(cfgPath, []byte(updated), 0o644))
}

// releaseEntry captures one " <hash> <size> <path>" line.
type releaseEntry struct {
	hash string
	size int64
}

// releaseEntriesFor scans a Release file and returns one entry per
// MD5Sum/SHA1/SHA256/SHA512 block whose path equals target. It is intentionally
// minimal — covers the entry shape produced by writeChecksumBlocks in
// pkg/index/release.go.
func releaseEntriesFor(t *testing.T, body, target string) map[string]releaseEntry {
	t.Helper()
	out := map[string]releaseEntry{}
	var current string
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") {
			if current == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != target {
				continue
			}
			size, err := strconv.ParseInt(fields[1], 10, 64)
			require.NoError(t, err)
			out[current] = releaseEntry{hash: fields[0], size: size}
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		if val == "" {
			current = key
		} else {
			current = ""
		}
	}
	return out
}

// headerValue returns the value of a Release header line whose key is exactly
// name (e.g. "Architectures"). Continuation/indented lines are not headers.
func headerValue(t *testing.T, body, name string) string {
	t.Helper()
	prefix := name + ":"
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("Release header %q not found", name)
	return ""
}

// readStanzas reads a Packages file and splits it into stanza strings.
func readStanzas(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return splitPackagesStanzas(body)
}

// splitPackagesStanzas splits a Packages body into stanzas. An empty body
// yields an empty slice rather than a single empty stanza.
func splitPackagesStanzas(body []byte) []string {
	trimmed := strings.TrimRight(string(body), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n\n")
}

// hasStanza reports whether any stanza in stanzas describes (name, version, arch).
// Match is anchored on full Package/Version/Architecture lines so prefix matches
// (alpha vs alpha-doc) don't fool it.
func hasStanza(stanzas []string, name, version, arch string) bool {
	wantName := "Package: " + name + "\n"
	wantVer := "Version: " + version + "\n"
	wantArch := "Architecture: " + arch + "\n"
	for _, s := range stanzas {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if strings.Contains(s, wantName) && strings.Contains(s, wantVer) && strings.Contains(s, wantArch) {
			return true
		}
	}
	return false
}

// buildTestDeb generates a minimal .deb at <dir>/<name>_<version>_<arch>.deb
// and returns its absolute path. Used by integration tests to avoid committing
// a combinatorial set of binary fixtures while still exercising deb.Inspect.
func buildTestDeb(t *testing.T, dir, name, version, arch string) string {
	t.Helper()

	control := fmt.Sprintf(
		"Package: %s\nVersion: %s\nArchitecture: %s\n"+
			"Maintainer: repokit tests <test@example.invalid>\n"+
			"Description: %s integration fixture (%s/%s)\n",
		name, version, arch, name, version, arch)

	controlTar, err := buildTestTar("control", []byte(control))
	require.NoError(t, err)
	controlGz, err := gzipBytes(controlTar)
	require.NoError(t, err)

	dataTar, err := buildTestTar("usr/share/"+name+".txt", []byte("hello\n"))
	require.NoError(t, err)
	dataGz, err := gzipBytes(dataTar)
	require.NoError(t, err)

	out := filepath.Join(dir, fmt.Sprintf("%s_%s_%s.deb", name, version, arch))
	f, err := os.Create(out)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	w := ar.NewWriter(f)
	require.NoError(t, w.WriteGlobalHeader())

	mtime := time.Unix(1700000000, 0).UTC()
	require.NoError(t, writeArMember(w, "debian-binary", []byte("2.0\n"), mtime))
	require.NoError(t, writeArMember(w, "control.tar.gz", controlGz, mtime))
	require.NoError(t, writeArMember(w, "data.tar.gz", dataGz, mtime))
	return out
}

// buildTestTar produces a tar archive containing a single regular file at
// "./<filename>", with parent "./" and any intermediate directories in
// filename also emitted as TypeDir entries (mirrors what dpkg-deb produces).
func buildTestTar(filename string, body []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Unix(1700000000, 0).UTC()

	if err := tw.WriteHeader(&tar.Header{
		Name: "./", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: mtime,
	}); err != nil {
		return nil, fmt.Errorf("tar root header: %w", err)
	}
	parts := strings.Split(filename, "/")
	parents := parts[:len(parts)-1]
	for i := range parents {
		dir := "./" + strings.Join(parents[:i+1], "/") + "/"
		if err := tw.WriteHeader(&tar.Header{
			Name: dir, Mode: 0o755, Typeflag: tar.TypeDir, ModTime: mtime,
		}); err != nil {
			return nil, fmt.Errorf("tar dir header %s: %w", dir, err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "./" + filename, Mode: 0o644, Size: int64(len(body)),
		Typeflag: tar.TypeReg, ModTime: mtime,
	}); err != nil {
		return nil, fmt.Errorf("tar file header %s: %w", filename, err)
	}
	if _, err := tw.Write(body); err != nil {
		return nil, fmt.Errorf("tar body %s: %w", filename, err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), nil
}

// gzipBytes wraps raw in a fresh gzip stream.
func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// writeArMember appends one ar member with body. ModTime/uid/gid are fixed for
// reproducibility.
func writeArMember(w *ar.Writer, name string, body []byte, mtime time.Time) error {
	hdr := &ar.Header{
		Name: name, ModTime: mtime, Uid: 0, Gid: 0, Mode: 0o100644, Size: int64(len(body)),
	}
	if err := w.WriteHeader(hdr); err != nil {
		return fmt.Errorf("ar header %s: %w", name, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("ar body %s: %w", name, err)
	}
	return nil
}
