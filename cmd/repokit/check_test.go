package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alkk/repokit/pkg/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCheck invokes the check subcommand with the supplied flags after the
// global --repo and --public flags. Returns exit code, stdout, stderr.
func runCheck(t *testing.T, state, public string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", state, "--public", public, "check"}, args...)
	var stdout, stderr bytes.Buffer
	code := run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

func TestCheckCleanRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.NotContains(t, stdout, "mismatch")
	assert.NotContains(t, stdout, "orphan")
	assert.NotContains(t, stdout, "missing")
}

func TestCheckTamperedFileDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	tampered := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(tampered, []byte("not a real deb"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "mismatch debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 mismatch(es)")
}

func TestCheckMissingPoolFileDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 missing")
	assert.Equal(t, 1, rowCount(t, tmpState), "check never removes rows for missing pool files")
}

// TestCheckOrphanDetected pins the post-cutover contract: check is
// read-only, so an orphan is reported and the run exits non-zero (the
// operator's signal to run `repokit gc`); the file is never touched.
func TestCheckOrphanDetected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.NotContains(t, stdout, "(removed)", "check must never report orphans as removed")
	assert.Contains(t, stdout, "1 orphan(s)")

	assert.FileExists(t, orphan, "check is read-only and must leave orphans on disk")
}

// TestCheckGCFlagRejected guards the breaking removal: `check --gc` no
// longer exists, so go-flags must reject it as an unknown flag (exitUsage)
// rather than silently doing nothing.
func TestCheckGCFlagRejected(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runCheck(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr, "unknown flag")
	assert.Contains(t, stderr, "gc")
}

func TestCheckScopedToSuiteIgnoresOtherSuites(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")

	otherOrphan := filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherOrphan), 0o755))
	require.NoError(t, os.WriteFile(otherOrphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "debian", "--suite", "bookworm")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.NotContains(t, stdout, "ghost", "out-of-scope orphan must not be reported")
	assert.FileExists(t, otherOrphan)
}

func TestCheckSuiteWithoutDistroErrors(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runCheck(t, tmpState, tmpPublic, "--suite", "bookworm")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "--suite requires --distro")
}

func TestCheckEmptyRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
}

// TestCheckScopedToDistroCoversAllSuites guards the middle branch of
// scopePoolDir (distro set, suite empty): distro-scoped check must report
// orphans from every suite under that distro and never reach into another
// distro. check is read-only, so every planted file survives.
func TestCheckScopedToDistroCoversAllSuites(t *testing.T) {
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

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "debian")
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ghost-book", "distro-scoped check must cover bookworm")
	assert.Contains(t, stdout, "ghost-bull", "distro-scoped check must cover bullseye")
	assert.NotContains(t, stdout, "ghost-noble", "check --distro debian must not touch ubuntu")
	assert.Contains(t, stdout, "2 orphan(s)")

	assert.FileExists(t, bookOrphan, "check must not remove in-scope orphans")
	assert.FileExists(t, bullOrphan, "check must not remove in-scope orphans")
	assert.FileExists(t, otherDistroOrphan, "check must not touch another distro's tree")
}

// TestCheckUnscopedAcrossDistrosFindsOrphans guards the multi-distro
// unscoped path: with no --distro flag, check must enumerate every
// configured distro and report orphans from all of them.
func TestCheckUnscopedAcrossDistrosFindsOrphans(t *testing.T) {
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

	code, out, errOut := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", errOut)
	assert.Contains(t, out, "orphan debian/pool/bookworm/main/g/ghost/ghost-deb_1.0_amd64.deb")
	assert.Contains(t, out, "orphan ubuntu/pool/noble/main/g/ghost/ghost-ubu_1.0_amd64.deb")
	assert.FileExists(t, debianOrphan, "check is read-only")
	assert.FileExists(t, ubuntuOrphan, "check is read-only")
}

// TestCheckUnscopedFindsOrphansInDBEmptyDistro guards against the
// regression where unscoped check silently misses orphan files in a
// configured distro that has no rows at all. Every distro listed in
// config.distros is walked even when the DB has no rows for it.
func TestCheckUnscopedFindsOrphansInDBEmptyDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	orphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan ubuntu/pool/noble/main/g/ghost/ghost_1.0_amd64.deb")
	assert.FileExists(t, orphan, "check must report but never reclaim orphans")
}

func TestCheckRejectsUnsafeScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	sentinel := filepath.Join(tmpPublic, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("must survive"), 0o644))

	code, _, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "..")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "--distro")
	assert.FileExists(t, sentinel, "sentinel outside pool/ must not be touched by check")
}

// TestCheckDoesNotTouchStateDir is a focused regression check that check's
// pool walk reads ctx.PublicDir, not ctx.Root: a same-named decoy orphan
// planted under the state-dir mirror layout must be invisible to check.
func TestCheckDoesNotTouchStateDir(t *testing.T) {
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

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.NotContains(t, stdout, "ghost", "check must not visit pool trees inside the state dir")

	assert.FileExists(t, stateDecoy, "check must not visit anything under the state dir")
	assert.FileExists(t, filepath.Join(tmpState, configFilename), "config.yaml must remain untouched")
	assert.FileExists(t, filepath.Join(tmpState, dbFilename), "repo.db must remain untouched")

	statePost, err := os.ReadDir(tmpState)
	require.NoError(t, err)
	assert.Len(t, statePost, len(statePre), "state dir top-level entry count must not change")
}

// TestCheckLeavesUnconfiguredDistroAlone is the central regression test for
// the explicit-distros gating: a public dir holding an unrelated top-level
// tree (e.g. raspbian/pool/... managed by another tool) must be invisible to
// check when raspbian is not in config.distros.
func TestCheckLeavesUnconfiguredDistroAlone(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	foreign := filepath.Join(tmpPublic, "raspbian", "pool", "bullseye", "main", "f", "foreign",
		"foreign_1.0_armhf.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o755))
	require.NoError(t, os.WriteFile(foreign, []byte("not repokit's"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.NotContains(t, stdout, "raspbian", "check must never walk unconfigured trees")

	assert.FileExists(t, foreign, "check must not touch unconfigured distros under publicDir")
	body, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, "not repokit's", string(body), "file contents must be untouched")
}

// TestCheckUnconfiguredDistroFlagAborts confirms a --distro value that is
// not in config.distros aborts the run before any pool walk happens.
func TestCheckUnconfiguredDistroFlagAborts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "raspbian")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "raspbian")
	assert.Contains(t, stderr, "config.distros")
	assert.FileExists(t, orphan, "check must abort before any pool walk when --distro is unconfigured")
}

// TestCheckUnscopedRejectsEmptyDistros guards the hard-cutover promise: an
// existing repo whose config.yaml has no distros: block must fail with a
// clear error when run unscoped, not silently succeed with "check: ok".
func TestCheckUnscopedRejectsEmptyDistros(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	body := "signing_key: ABC123\npublic_dir: \"" + tmpPublic + "\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpState, configFilename), []byte(body), 0o600))

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "config.distros is empty")
	assert.FileExists(t, orphan, "check must abort before any pool walk when config.distros is empty")
}

// TestCheckUnscopedSkipsRowsForUnconfiguredDistro guards the check
// counterpart to publish's belt-and-braces filter: if a DB row exists for a
// distro that was later removed from config.distros, check must skip the row
// (with a stderr warning) rather than open files under an unconfigured tree.
func TestCheckUnscopedSkipsRowsForUnconfiguredDistro(t *testing.T) {
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

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.Contains(t, stderr, `"raspbian"`,
		"stderr must warn that the unconfigured distro was skipped")
	assert.Contains(t, stderr, "config.distros",
		"stderr warning must point at the config field")

	assert.FileExists(t, raspbianPool,
		"check must not touch pool files for distros missing from config.distros")
	bodyAfter, err := os.ReadFile(raspbianPool)
	require.NoError(t, err)
	assert.Equal(t, bodyBefore, bodyAfter, "raspbian pool file contents must be unchanged")
}

// TestCheckDoesNotReportPoolWriteArtifactsAsOrphans guards against check
// flagging the recoverable copies left by an interrupted pool.Replace. A
// crashed Replace can leave dest.bak (old bytes) and dest.tmp (new bytes) on
// disk while the DB row still references dest; reporting these as orphans
// would mislead the operator into gc-ing a recoverable state.
func TestCheckDoesNotReportPoolWriteArtifactsAsOrphans(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	dest := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(dest+".bak", []byte("old bytes"), 0o644))
	require.NoError(t, os.WriteFile(dest+".tmp", []byte("new bytes"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
	assert.NotContains(t, stdout, ".bak", "pool-write .bak must not be reported as orphan")
	assert.NotContains(t, stdout, ".tmp", "pool-write .tmp must not be reported as orphan")
	assert.FileExists(t, dest+".bak", "check must preserve .bak (it is read-only)")
	assert.FileExists(t, dest+".tmp", "check must preserve .tmp (it is read-only)")
}

// TestCheckAcceptsNamespacedDistro covers the namespaced --distro validator:
// with distros: [customer/acme] declared and a matching row in the DB,
// `check --distro customer/acme` must pass validateDistroScope and
// requireConfiguredDistro, then check the namespaced pool file.
func TestCheckAcceptsNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")

	pkg := fakePackage(t, tmpPublic, "customer/acme", "stable", "amd64", "hello", "1.0")
	insertPackage(t, tmpState, pkg)

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "customer/acme")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: ok")
}

// rowsMatching opens the repo DB and returns the rows matching filter. Used
// by the --purge-db tests to assert which rows survived a purge.
func rowsMatching(t *testing.T, state string, filter repo.Filter) []repo.Package {
	t.Helper()
	r, err := repo.Open(context.Background(), filepath.Join(state, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	rows, err := r.List(context.Background(), filter)
	require.NoError(t, err)
	return rows
}

// TestCheck_PurgeDB_RemovesMissingRows pins the core Mode B contract: a row
// whose pool file is gone is deleted, the report prints `purged X`, and the
// post-purge scope is clean so the run exits 0.
func TestCheck_PurgeDB_RemovesMissingRows(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 0 orphan(s)")
	assert.NotContains(t, stdout, "missing debian", "missing rows must be reported as purged, not missing")
	assert.Equal(t, 0, rowCount(t, tmpState), "the missing-file row must be deleted")
}

// TestCheck_PurgeDB_LeavesMismatchesAlone guards the missing-only scope: a
// corrupted (mismatched) pool file is reported but never purged, because a
// transient FS glitch can mimic corruption — dropping the row would mask it.
func TestCheck_PurgeDB_LeavesMismatchesAlone(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	tampered := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(tampered, []byte("not a real deb"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "mismatch debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "check: 1 mismatch(es), 0 purged, 0 orphan(s)")
	assert.Equal(t, 1, rowCount(t, tmpState), "--purge-db must never delete a mismatched row")
	assert.FileExists(t, tampered, "--purge-db must not touch pool files")
}

// TestCheck_PurgeDB_ScopedByDistroSuite pins that --purge-db respects the
// existing --distro filter: an out-of-scope distro's missing-file row must
// survive even though its pool file is also gone.
func TestCheck_PurgeDB_ScopedByDistroSuite(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	var addOut, addErr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "ubuntu", "--suite", "noble",
		fixtureDeb(t, "hello-gz_1.0_amd64.deb"),
	}, &addOut, &addErr, "test-rev")
	require.Equal(t, exitOK, code, "ubuntu add failed: %s", addErr.String())

	debTarget := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	ubuTarget := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(debTarget))
	require.NoError(t, os.Remove(ubuTarget))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--distro", "debian", "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.NotContains(t, stdout, "ubuntu", "out-of-scope distro must not be reported or touched")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 0 orphan(s)")

	assert.Empty(t, rowsMatching(t, tmpState, repo.Filter{Distro: "debian"}),
		"debian's missing-file row must be purged")
	assert.Len(t, rowsMatching(t, tmpState, repo.Filter{Distro: "ubuntu"}), 1,
		"ubuntu row is out of scope and must remain")
}

// TestCheck_PurgeDB_NoMissingNoOp pins the no-op path: with nothing missing,
// --purge-db still prints the count line (like gc's "removed 0") and writes
// nothing to the DB.
func TestCheck_PurgeDB_NoMissingNoOp(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "check: 0 mismatch(es), 0 purged, 0 orphan(s)")
	assert.NotContains(t, stdout, "purged debian", "nothing was missing, so nothing is purged")
	assert.Equal(t, 1, rowCount(t, tmpState), "a clean --purge-db must not delete anything")
}

// TestCheck_PurgeDB_PurgesEveryMissingRowAcrossSuites pins that the same NVRA
// added to two suites yields two rows backed by two distinct pool files (the
// pool path embeds the suite); when both files are gone, a single --purge-db
// pass deletes both rows, each by its own full-key DeleteConn.
func TestCheck_PurgeDB_PurgesEveryMissingRowAcrossSuites(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")
	require.Equal(t, 2, rowCount(t, tmpState))

	for _, suite := range []string{"bookworm", "bullseye"} {
		target := filepath.Join(tmpPublic, "debian", "pool", suite, "main", "h", "hello-gz",
			"hello-gz_1.0_amd64.deb")
		require.NoError(t, os.Remove(target))
	}

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "purged debian/pool/bullseye/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 2 purged, 0 orphan(s)")
	assert.Equal(t, 0, rowCount(t, tmpState),
		"every missing-file row across suites is purged in one pass")
}

// TestCheck_PurgeDB_LeavesIntactSiblingSuiteRow is the regression guard for
// the over-deletion bug. The same NVRA in two suites is two distinct pool
// files and two rows. Removing only one suite's file must purge only that
// row; the sibling suite's row — whose file is present and still referenced
// by its published indexes — must survive.
func TestCheck_PurgeDB_LeavesIntactSiblingSuiteRow(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")
	require.Equal(t, 2, rowCount(t, tmpState))

	bullseyeFile := filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	bookwormFile := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(bookwormFile))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.NotContains(t, stdout, "purged debian/pool/bullseye",
		"the sibling suite's row, whose file is intact, must not be purged")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 0 orphan(s)")

	assert.Len(t, rowsMatching(t, tmpState, repo.Filter{Distro: "debian", Suite: "bullseye"}), 1,
		"the intact bullseye row must survive")
	assert.Empty(t, rowsMatching(t, tmpState, repo.Filter{Distro: "debian", Suite: "bookworm"}),
		"only the missing-file bookworm row must be purged")
	assert.FileExists(t, bullseyeFile, "--purge-db must not touch pool files")
}

// TestCheck_PurgeDB_SuiteScopedPurgesOnlyInScope pins --suite scoping for
// Mode B: with both suites' files gone but the run scoped to one suite, only
// the in-scope row is purged and the out-of-scope row survives.
func TestCheck_PurgeDB_SuiteScopedPurgesOnlyInScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")
	require.Equal(t, 2, rowCount(t, tmpState))

	for _, suite := range []string{"bookworm", "bullseye"} {
		target := filepath.Join(tmpPublic, "debian", "pool", suite, "main", "h", "hello-gz",
			"hello-gz_1.0_amd64.deb")
		require.NoError(t, os.Remove(target))
	}

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm", "--purge-db")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.NotContains(t, stdout, "bullseye", "out-of-scope suite must not be reported or touched")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 0 orphan(s)")

	assert.Empty(t, rowsMatching(t, tmpState, repo.Filter{Distro: "debian", Suite: "bookworm"}),
		"the in-scope bookworm row must be purged")
	assert.Len(t, rowsMatching(t, tmpState, repo.Filter{Distro: "debian", Suite: "bullseye"}), 1,
		"the out-of-scope bullseye row must survive")
}

// TestCheck_PurgeDB_MismatchAndMissingInOneRun pins the post-action exit rule
// when both findings coexist: the missing-file row is purged (resolved) while
// the mismatched row is reported and retained, and the run still exits 1
// because the unresolved mismatch keeps the scope non-clean.
func TestCheck_PurgeDB_MismatchAndMissingInOneRun(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	missing := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(missing))
	tampered := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello",
		"hello-xz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(tampered, []byte("not a real deb"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "mismatch debian/pool/bookworm/main/h/hello/hello-xz_1.0_amd64.deb")
	assert.Contains(t, stdout, "check: 1 mismatch(es), 1 purged, 0 orphan(s)")
	assert.Len(t, rowsMatching(t, tmpState, repo.Filter{Name: "hello-xz"}), 1,
		"the mismatched row must be retained")
	assert.Empty(t, rowsMatching(t, tmpState, repo.Filter{Name: "hello-gz"}),
		"the missing-file row must be purged")
}

// TestCheck_PurgeDB_HardOpenErrorDoesNotPurge guards the missing-only safety
// contract against a non-ErrNotExist error: a pool path that exists but is
// unreadable as a file (here, replaced by a directory) is a hard error, not
// an "absent" finding, so the run aborts non-zero and the row is NOT purged.
func TestCheck_PurgeDB_HardOpenErrorDoesNotPurge(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Mkdir(target, 0o755))

	code, _, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "check:", "a hard hash/open error must surface on stderr")
	assert.Equal(t, 1, rowCount(t, tmpState),
		"a non-ErrNotExist open/hash error must abort the purge, not delete the row")
}

// TestCheck_PurgeDB_OrphanReportingDuringPurge validates the pre-purge
// known-set decision: a genuine orphan staged alongside a missing-file row is
// still reported (and keeps the run non-clean) even though the purged row's
// path stays in the known set used by the orphan scan.
func TestCheck_PurgeDB_OrphanReportingDuringPurge(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	missing := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(missing))

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 1 orphan(s)")
	assert.Equal(t, 0, rowCount(t, tmpState), "the missing-file row is purged even with an orphan present")
	assert.FileExists(t, orphan, "--purge-db must not remove orphan files (that is gc's job)")
}

// TestCheck_PurgeDB_RestoredFileMidRunSurfacesAsOrphan pins the false-negative
// fix: the writer lock only serializes DB writers, so an external process can
// recreate a previously-missing pool file between the hash pass and the orphan
// scan. Its row is purged, so the reappeared file is a genuine orphan and must
// be reported (run stays non-clean) rather than suppressed by a stale known
// entry. purgeScanHook injects the restore deterministically in that window.
func TestCheck_PurgeDB_RestoredFileMidRunSurfacesAsOrphan(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	pf := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(pf))

	purgeScanHook = func() {
		// Simulate a backup restore / re-synced replica recreating the file
		// after its row has been purged but before the orphan scan walks.
		_ = os.WriteFile(pf, []byte("restored-out-of-band"), 0o644)
	}
	t.Cleanup(func() { purgeScanHook = nil })

	code, stdout, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
	require.Equal(t, exitError, code, "a fresh on-disk orphan must keep the run non-clean; stderr: %s", stderr)
	assert.Contains(t, stdout, "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb",
		"the row-less restored file must be reported, not suppressed by the pre-purge known set")
	assert.Contains(t, stdout, "check: 0 mismatch(es), 1 purged, 1 orphan(s)")
	assert.Equal(t, 0, rowCount(t, tmpState), "the missing-file row is still purged")
	assert.FileExists(t, pf, "--purge-db must not remove orphan files (that is gc's job)")
}

// TestCheck_PurgeDB_QuietSuppressesSummaryNotPurgedLines pins the tool-wide
// --quiet contract for Mode B (mirrors TestGcQuietSuppressesSummaryNotOrphan-
// Lines): the trailing "check: ... purged ..." aggregate is suppressed, but
// the per-row "purged <path>" lines still print so a quiet purge stays
// scriptable.
func TestCheck_PurgeDB_QuietSuppressesSummaryNotPurgedLines(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	pf := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(pf))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "--quiet", "check", "--purge-db",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "purged debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb",
		"--quiet must still print per-row purged lines")
	assert.NotContains(t, stdout.String(), "check:", "--quiet must suppress the trailing summary line")
	assert.Equal(t, 0, rowCount(t, tmpState), "the missing-file row is purged under --quiet")
}

// TestCheck_PurgeDB_SerializesWithConcurrentWriter mirrors
// TestGcSerializesWithConcurrentWriter: Mode B takes the writer lock, so a
// concurrently held lock (mimicking an in-flight add's INSERT-to-COMMIT
// window) must block check --purge-db until release.
func TestCheck_PurgeDB_SerializesWithConcurrentWriter(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	r, err := repo.Open(context.Background(), filepath.Join(tmpState, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	holderReady := make(chan struct{})
	releaseHolder := make(chan struct{})

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = r.WithWriterLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
			close(holderReady)
			<-releaseHolder
			return nil
		})
	})

	<-holderReady

	checkDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		code, _, stderr := runCheck(t, tmpState, tmpPublic, "--purge-db")
		checkDone <- struct {
			code   int
			stderr string
		}{code, stderr}
	}()

	select {
	case res := <-checkDone:
		t.Fatalf("check --purge-db completed while writer lock was held: code=%d stderr=%s",
			res.code, res.stderr)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	wg.Wait()

	select {
	case res := <-checkDone:
		require.Equal(t, exitOK, res.code, "stderr: %s", res.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("check --purge-db did not finish after writer lock release")
	}
}
