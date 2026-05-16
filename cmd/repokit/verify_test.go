package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runVerify invokes the verify subcommand with the supplied flags after the
// global --repo and --public flags. Returns exit code, stdout, stderr.
func runVerify(t *testing.T, state, public string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", state, "--public", public, "verify"}, args...)
	var stdout, stderr bytes.Buffer
	code := run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

func TestVerifyCleanRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "mismatch")
	assert.NotContains(t, stdout, "orphan")
	assert.NotContains(t, stdout, "missing")
}

func TestVerifyTamperedFileDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	tampered := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(tampered, []byte("not a real deb"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "mismatch debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 mismatch(es)")
}

func TestVerifyMissingPoolFileDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 missing")
	assert.Equal(t, 1, rowCount(t, tmpState), "verify never removes rows for missing pool files")
}

// TestVerifyOrphanDetected pins the post-cutover contract: verify is
// read-only, so an orphan is reported and the run exits non-zero (the
// operator's signal to run `repokit gc`); the file is never touched.
func TestVerifyOrphanDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.NotContains(t, stdout, "(removed)", "verify must never report orphans as removed")
	assert.Contains(t, stdout, "1 orphan(s)")

	assert.FileExists(t, orphan, "verify is read-only and must leave orphans on disk")
}

// TestVerifyGCFlagRejected guards the breaking removal: `verify --gc` no
// longer exists, so go-flags must reject it as an unknown flag (exitUsage)
// rather than silently doing nothing.
func TestVerifyGCFlagRejected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr, "unknown flag")
	assert.Contains(t, stderr, "gc")
}

func TestVerifyScopedToSuiteIgnoresOtherSuites(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")

	otherOrphan := filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherOrphan), 0o755))
	require.NoError(t, os.WriteFile(otherOrphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "debian", "--suite", "bookworm")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "ghost", "out-of-scope orphan must not be reported")
	assert.FileExists(t, otherOrphan)
}

func TestVerifySuiteWithoutDistroErrors(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--suite", "bookworm")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "--suite requires --distro")
}

func TestVerifyEmptyRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
}

// TestVerifyScopedToDistroCoversAllSuites guards the middle branch of
// scopePoolDir (distro set, suite empty): distro-scoped verify must report
// orphans from every suite under that distro and never reach into another
// distro. Verify is read-only, so every planted file survives.
func TestVerifyScopedToDistroCoversAllSuites(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")

	bookOrphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost-book_1.0_amd64.deb")
	bullOrphan := filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main", "g", "ghost",
		"ghost-bull_1.0_amd64.deb")
	for _, p := range []string{bookOrphan, bullOrphan} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("orphan"), 0o644))
	}
	otherDistroOrphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost-noble_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherDistroOrphan), 0o755))
	require.NoError(t, os.WriteFile(otherDistroOrphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "debian")
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ghost-book", "distro-scoped verify must cover bookworm")
	assert.Contains(t, stdout, "ghost-bull", "distro-scoped verify must cover bullseye")
	assert.NotContains(t, stdout, "ghost-noble", "verify --distro debian must not touch ubuntu")
	assert.Contains(t, stdout, "2 orphan(s)")

	assert.FileExists(t, bookOrphan, "verify must not remove in-scope orphans")
	assert.FileExists(t, bullOrphan, "verify must not remove in-scope orphans")
	assert.FileExists(t, otherDistroOrphan, "verify must not touch another distro's tree")
}

// TestVerifyUnscopedAcrossDistrosFindsOrphans guards the multi-distro
// unscoped path: with no --distro flag, verify must enumerate every
// configured distro and report orphans from all of them.
func TestVerifyUnscopedAcrossDistrosFindsOrphans(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "ubuntu", "--suite", "noble",
		fixtureDeb(t, "hello-gz_1.0_amd64.deb"),
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "fixture add for ubuntu failed: %s", stderr.String())

	debianOrphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost-deb_1.0_amd64.deb")
	ubuntuOrphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost-ubu_1.0_amd64.deb")
	for _, p := range []string{debianOrphan, ubuntuOrphan} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("orphan"), 0o644))
	}

	code, out, errOut := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", errOut)
	assert.Contains(t, out, "orphan debian/pool/bookworm/main/g/ghost/ghost-deb_1.0_amd64.deb")
	assert.Contains(t, out, "orphan ubuntu/pool/noble/main/g/ghost/ghost-ubu_1.0_amd64.deb")
	assert.FileExists(t, debianOrphan, "verify is read-only")
	assert.FileExists(t, ubuntuOrphan, "verify is read-only")
}

// TestVerifyUnscopedFindsOrphansInDBEmptyDistro guards against the
// regression where unscoped verify silently misses orphan files in a
// configured distro that has no rows at all. Every distro listed in
// config.distros is walked even when the DB has no rows for it.
func TestVerifyUnscopedFindsOrphansInDBEmptyDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	orphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan ubuntu/pool/noble/main/g/ghost/ghost_1.0_amd64.deb")
	assert.FileExists(t, orphan, "verify must report but never reclaim orphans")
}

func TestVerifyRejectsUnsafeScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	sentinel := filepath.Join(tmpPublic, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("must survive"), 0o644))

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "..")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "--distro")
	assert.FileExists(t, sentinel, "sentinel outside pool/ must not be touched by verify")
}

// TestVerifyDoesNotTouchStateDir is a focused regression check that verify's
// pool walk reads ctx.PublicDir, not ctx.Root: a same-named decoy orphan
// planted under the state-dir mirror layout must be invisible to verify.
func TestVerifyDoesNotTouchStateDir(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	stateDecoy := filepath.Join(tmpState, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDecoy), 0o755))
	require.NoError(t, os.WriteFile(stateDecoy, []byte("decoy under state"), 0o644))

	statePre, err := os.ReadDir(tmpState)
	require.NoError(t, err)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "ghost", "verify must not visit pool trees inside the state dir")

	assert.FileExists(t, stateDecoy, "verify must not visit anything under the state dir")
	assert.FileExists(t, filepath.Join(tmpState, configFilename), "config.yaml must remain untouched")
	assert.FileExists(t, filepath.Join(tmpState, dbFilename), "repo.db must remain untouched")

	statePost, err := os.ReadDir(tmpState)
	require.NoError(t, err)
	assert.Len(t, statePost, len(statePre), "state dir top-level entry count must not change")
}

// TestVerifyLeavesUnconfiguredDistroAlone is the central regression test for
// the explicit-distros gating: a public dir holding an unrelated top-level
// tree (e.g. raspbian/pool/... managed by another tool) must be invisible to
// verify when raspbian is not in config.distros.
func TestVerifyLeavesUnconfiguredDistroAlone(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	foreign := filepath.Join(tmpPublic, "raspbian", "pool", "bullseye", "main", "f", "foreign",
		"foreign_1.0_armhf.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o755))
	require.NoError(t, os.WriteFile(foreign, []byte("not repokit's"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "raspbian", "verify must never walk unconfigured trees")

	assert.FileExists(t, foreign, "verify must not touch unconfigured distros under publicDir")
	body, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, "not repokit's", string(body), "file contents must be untouched")
}

// TestVerifyUnconfiguredDistroFlagAborts confirms a --distro value that is
// not in config.distros aborts the run before any pool walk happens.
func TestVerifyUnconfiguredDistroFlagAborts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "raspbian")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "raspbian")
	assert.Contains(t, stderr, "config.distros")
	assert.FileExists(t, orphan, "verify must abort before any pool walk when --distro is unconfigured")
}

// TestVerifyUnscopedRejectsEmptyDistros guards the hard-cutover promise: an
// existing repo whose config.yaml has no distros: block must fail with a
// clear error when run unscoped, not silently succeed with "verify: ok".
func TestVerifyUnscopedRejectsEmptyDistros(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	body := "signing_key: ABC123\npublic_dir: \"" + tmpPublic + "\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpState, configFilename), []byte(body), 0o600))

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "config.distros is empty")
	assert.FileExists(t, orphan, "verify must abort before any pool walk when config.distros is empty")
}

// TestVerifyUnscopedSkipsRowsForUnconfiguredDistro guards the verify
// counterpart to publish's belt-and-braces filter: if a DB row exists for a
// distro that was later removed from config.distros, verify must skip the row
// (with a stderr warning) rather than open files under an unconfigured tree.
func TestVerifyUnscopedSkipsRowsForUnconfiguredDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	insertPackage(t, tmpState, fakePackage(t, tmpPublic, "debian", "bookworm", "amd64", "alpha", "1.0"))
	insertPackage(t, tmpState, fakePackage(t, tmpPublic, "raspbian", "bullseye", "armhf", "beta", "1.0"))

	raspbianPool := filepath.Join(tmpPublic, "raspbian", "pool", "bullseye", "main", "b", "beta",
		"beta_1.0_armhf.deb")
	require.FileExists(t, raspbianPool)
	bodyBefore, err := os.ReadFile(raspbianPool)
	require.NoError(t, err)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.Contains(t, stderr, `"raspbian"`,
		"stderr must warn that the unconfigured distro was skipped")
	assert.Contains(t, stderr, "config.distros",
		"stderr warning must point at the config field")

	assert.FileExists(t, raspbianPool,
		"verify must not touch pool files for distros missing from config.distros")
	bodyAfter, err := os.ReadFile(raspbianPool)
	require.NoError(t, err)
	assert.Equal(t, bodyBefore, bodyAfter, "raspbian pool file contents must be unchanged")
}

// TestVerifyDoesNotReportPoolWriteArtifactsAsOrphans guards against verify
// flagging the recoverable copies left by an interrupted pool.Replace. A
// crashed Replace can leave dest.bak (old bytes) and dest.tmp (new bytes) on
// disk while the DB row still references dest; reporting these as orphans
// would mislead the operator into gc-ing a recoverable state.
func TestVerifyDoesNotReportPoolWriteArtifactsAsOrphans(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	dest := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(dest+".bak", []byte("old bytes"), 0o644))
	require.NoError(t, os.WriteFile(dest+".tmp", []byte("new bytes"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, ".bak", "pool-write .bak must not be reported as orphan")
	assert.NotContains(t, stdout, ".tmp", "pool-write .tmp must not be reported as orphan")
	assert.FileExists(t, dest+".bak", "verify must preserve .bak (it is read-only)")
	assert.FileExists(t, dest+".tmp", "verify must preserve .tmp (it is read-only)")
}

// TestVerifyAcceptsNamespacedDistro covers the namespaced --distro validator:
// with distros: [customer/acme] declared and a matching row in the DB,
// `verify --distro customer/acme` must pass validateDistroScope and
// requireConfiguredDistro, then check the namespaced pool file.
func TestVerifyAcceptsNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")

	pkg := fakePackage(t, tmpPublic, "customer/acme", "stable", "amd64", "hello", "1.0")
	insertPackage(t, tmpState, pkg)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "customer/acme")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
}
