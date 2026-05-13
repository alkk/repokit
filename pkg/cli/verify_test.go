package cli

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

// runVerify invokes the verify subcommand with the supplied flags after the
// global --repo and --public flags. Returns exit code, stdout, stderr.
func runVerify(t *testing.T, state, public string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", state, "--public", public, "verify"}, args...)
	var stdout, stderr bytes.Buffer
	code := Run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

func TestVerifyCleanRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
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
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
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
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 missing")
}

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
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 orphan(s)")

	assert.FileExists(t, orphan, "orphan must remain on disk without --gc")
}

func TestVerifyGCRemovesOnlyOrphan(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	legit := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb (removed)")
	assert.Contains(t, stdout, "verify: ok")

	assert.NoFileExists(t, orphan, "--gc must remove orphan")
	assert.FileExists(t, legit, "--gc must leave legit pool files in place")
	assert.Equal(t, 1, rowCount(t, tmpState), "--gc must not touch DB rows")
}

func TestVerifyGCDoesNotRemoveRowsForMissingPoolFiles(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing")
	assert.Equal(t, 1, rowCount(t, tmpState), "rows for missing pool files must survive --gc")
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
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "ghost", "out-of-scope orphan must not be reported")
	assert.FileExists(t, otherOrphan)
}

func TestVerifySuiteWithoutDistroErrors(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--suite", "bookworm")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "--suite requires --distro")
}

func TestVerifyEmptyRepoPasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
}

// TestVerifyScopedToDistroCoversAllSuites guards the middle branch of
// scopePoolDir (distro set, suite empty). Without it, a regression that
// collapsed the distro-only case to either the suite-scoped or root path would
// silently include or exclude the wrong subtree of orphans.
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

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "debian", "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ghost-book", "distro-scoped verify must cover bookworm")
	assert.Contains(t, stdout, "ghost-bull", "distro-scoped verify must cover bullseye")
	assert.NotContains(t, stdout, "ghost-noble", "verify --distro debian must not touch ubuntu")

	assert.NoFileExists(t, bookOrphan, "--gc must remove in-scope orphan from bookworm")
	assert.NoFileExists(t, bullOrphan, "--gc must remove in-scope orphan from bullseye")
	assert.FileExists(t, otherDistroOrphan, "--gc must leave out-of-scope orphan in another distro")
}

// TestVerifyGCPreservesPoolWriteArtifacts guards against verify --gc destroying
// the recoverable copy after an interrupted pool.Replace. A crashed Replace can
// leave dest.bak (old bytes) and dest.tmp (new bytes) on disk while the DB row
// still references dest; classifying these as orphans would turn a recoverable
// state into permanent data loss.
func TestVerifyGCPreservesPoolWriteArtifacts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	dest := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(dest+".bak", []byte("old bytes"), 0o644))
	require.NoError(t, os.WriteFile(dest+".tmp", []byte("new bytes"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.NotContains(t, stdout, ".bak", "pool-write .bak must not be reported as orphan")
	assert.NotContains(t, stdout, ".tmp", "pool-write .tmp must not be reported as orphan")
	assert.FileExists(t, dest+".bak", "--gc must preserve .bak so an interrupted Replace stays recoverable")
	assert.FileExists(t, dest+".tmp", "--gc must preserve .tmp so an interrupted Replace stays recoverable")
}

// TestVerifyGCSerializesWithConcurrentWriter guards the race where an
// in-flight `add` writes its pool bytes inside its own writer transaction
// but verify --gc, computing orphans against committed rows only, deletes
// the file before that transaction commits. Holding a writer lock from a
// separate connection (mimicking add's INSERT-to-COMMIT window) must block
// verify --gc until release.
func TestVerifyGCSerializesWithConcurrentWriter(t *testing.T) {
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

	verifyDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		code, _, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
		verifyDone <- struct {
			code   int
			stderr string
		}{code, stderr}
	}()

	select {
	case res := <-verifyDone:
		t.Fatalf("verify --gc completed while writer lock was held: code=%d stderr=%s", res.code, res.stderr)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	wg.Wait()

	select {
	case res := <-verifyDone:
		require.Equal(t, ExitOK, res.code, "stderr: %s", res.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("verify --gc did not finish after writer lock release")
	}
}

// TestVerifyUnscopedAcrossDistrosFindsOrphans guards the multi-distro unscoped
// path: with no --distro flag, verify must enumerate every distro and walk
// each per-distro pool tree, reporting orphans from all of them.
func TestVerifyUnscopedAcrossDistrosFindsOrphans(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "ubuntu", "--suite", "noble",
		fixtureDeb(t, "hello-gz_1.0_amd64.deb"),
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "fixture add for ubuntu failed: %s", stderr.String())

	debianOrphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost-deb_1.0_amd64.deb")
	ubuntuOrphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost-ubu_1.0_amd64.deb")
	for _, p := range []string{debianOrphan, ubuntuOrphan} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("orphan"), 0o644))
	}

	code, out, errOut := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", errOut)
	assert.Contains(t, out, "orphan debian/pool/bookworm/main/g/ghost/ghost-deb_1.0_amd64.deb (removed)")
	assert.Contains(t, out, "orphan ubuntu/pool/noble/main/g/ghost/ghost-ubu_1.0_amd64.deb (removed)")
	assert.NoFileExists(t, debianOrphan)
	assert.NoFileExists(t, ubuntuOrphan)
}

// TestVerifyUnscopedFindsOrphansInDBEmptyDistro guards against the
// regression where unscoped verify silently misses orphan files in a
// configured distro that has no rows at all (e.g. after a crashed add or
// a delete pass that removed the last row but failed to clean up the
// pool file). Every distro listed in config.distros is walked even when
// the DB has no rows for it.
func TestVerifyUnscopedFindsOrphansInDBEmptyDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	orphan := filepath.Join(tmpPublic, "ubuntu", "pool", "noble", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan ubuntu/pool/noble/main/g/ghost/ghost_1.0_amd64.deb (removed)")
	assert.NoFileExists(t, orphan, "--gc must reclaim orphans even when the distro has no DB rows")
}

func TestVerifyRejectsUnsafeScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	sentinel := filepath.Join(tmpPublic, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("must survive"), 0o644))

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--gc", "--distro", "..")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "--distro")
	assert.FileExists(t, sentinel, "sentinel outside pool/ must not be touched by verify --gc")
}

// TestVerifyDoesNotTouchStateDir is a focused regression check that verify's
// pool walks read from ctx.PublicDir, not ctx.Root: if a same-named decoy
// orphan file is planted under tmpState's mirrored layout, --gc must not see
// or remove it because state-dir contents are outside verify's search space.
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

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "ghost", "verify must not visit pool trees inside the state dir")

	assert.FileExists(t, stateDecoy, "verify --gc must not unlink anything under the state dir")
	assert.FileExists(t, filepath.Join(tmpState, configFilename), "config.yaml must remain untouched")
	assert.FileExists(t, filepath.Join(tmpState, dbFilename), "repo.db must remain untouched")

	statePost, err := os.ReadDir(tmpState)
	require.NoError(t, err)
	assert.Len(t, statePost, len(statePre), "state dir top-level entry count must not change")
}

// TestVerifyGCLeavesUnconfiguredDistroAlone is the central regression test
// for the explicit-distros bug fix: a public dir holding an unrelated
// top-level tree (e.g. raspbian/pool/... managed by another tool) must be
// invisible to verify --gc when raspbian is not in config.distros.
// Before the fix, the disk-discovery branch of unionWithDiskDistros would
// walk the tree, classify every file as an orphan, and delete it.
func TestVerifyGCLeavesUnconfiguredDistroAlone(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	foreign := filepath.Join(tmpPublic, "raspbian", "pool", "bullseye", "main", "f", "foreign",
		"foreign_1.0_armhf.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o755))
	require.NoError(t, os.WriteFile(foreign, []byte("not repokit's"), 0o644))

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "raspbian", "verify must never walk unconfigured trees")

	assert.FileExists(t, foreign, "verify --gc must not touch unconfigured distros under publicDir")
	body, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, "not repokit's", string(body), "file contents must be untouched")
}

// TestVerifyUnconfiguredDistroFlagAborts confirms a --distro value that
// is not in config.distros aborts the run before any pool walk happens.
// We plant a known-orphan inside a configured distro: if the run had
// proceeded past the gate, --gc would have deleted it; the assertion that
// the file survives proves the early abort.
func TestVerifyUnconfiguredDistroFlagAborts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--gc", "--distro", "raspbian")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "raspbian")
	assert.Contains(t, stderr, "config.distros")
	assert.FileExists(t, orphan, "verify must abort before any pool walk when --distro is unconfigured")
}

// TestVerifyUnscopedRejectsEmptyDistros guards the hard-cutover promise:
// an existing repo whose config.yaml has no distros: block (or an empty
// list) must fail with a clear error when run unscoped, not silently
// succeed with "verify: ok". The scoped --distro path is already covered
// by requireConfiguredDistro.
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

	code, _, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "config.distros is empty")
	assert.FileExists(t, orphan, "verify --gc must abort before any pool walk when config.distros is empty")
}

// TestVerifyUnscopedSkipsRowsForUnconfiguredDistro guards the verify
// counterpart to publish's belt-and-braces filter: if a DB row exists for
// a distro that was later removed from config.distros, verify must skip
// the row (with a stderr warning) rather than open files under an
// unconfigured tree.
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

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
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

// TestVerifyAcceptsNamespacedDistro covers the Task 3 repointing of verify's
// --distro validator. With distros: [customer/acme] declared and a matching
// row in the DB, `verify --distro customer/acme` must pass validateDistroScope
// and requireConfiguredDistro, then check the namespaced pool file.
func TestVerifyAcceptsNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")

	pkg := fakePackage(t, tmpPublic, "customer/acme", "stable", "amd64", "hello", "1.0")
	insertPackage(t, tmpState, pkg)

	code, stdout, stderr := runVerify(t, tmpState, tmpPublic, "--distro", "customer/acme")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
}
