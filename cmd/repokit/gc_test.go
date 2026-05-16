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

// runGc invokes the gc subcommand with the supplied flags after the global
// --repo and --public flags. Returns exit code, stdout, stderr.
func runGc(t *testing.T, state, public string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", state, "--public", public, "gc"}, args...)
	var stdout, stderr bytes.Buffer
	code := run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

func TestGcRemovesOrphanAndPrunesParents(t *testing.T) {
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

	code, stdout, stderr := runGc(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "removed debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.Contains(t, stdout, "gc: removed 1 orphan(s)")

	assert.NoFileExists(t, orphan, "gc must remove the orphan")
	assert.FileExists(t, legit, "gc must leave legit pool files in place")
	assert.Equal(t, 1, rowCount(t, tmpState), "gc must never touch DB rows")

	assert.NoDirExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost"),
		"emptied orphan source dir must be pruned")
	assert.NoDirExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g"),
		"emptied letter shard must be pruned when its only source dir is gone")
	assert.DirExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz"),
		"sibling source dir under a different letter shard must remain")
	assert.DirExists(t, filepath.Join(tmpPublic, "debian", "pool"),
		"per-distro pool root must never be pruned (stop boundary)")
}

// TestGcScopedToDistroLeavesOthersAlone proves --distro narrows the walk to
// one distro: orphans in another configured distro must survive untouched.
func TestGcScopedToDistroLeavesOthersAlone(t *testing.T) {
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

	code, out, errOut := runGc(t, tmpState, tmpPublic, "--distro", "debian")
	require.Equal(t, exitOK, code, "stderr: %s", errOut)
	assert.Contains(t, out, "removed debian/pool/bookworm/main/g/ghost/ghost-deb_1.0_amd64.deb")
	assert.NotContains(t, out, "ghost-ubu", "gc --distro debian must not touch ubuntu")

	assert.NoFileExists(t, debianOrphan, "in-scope orphan must be removed")
	assert.FileExists(t, ubuntuOrphan, "out-of-scope orphan in another distro must survive")
}

// TestGcScopedToSuiteIgnoresOtherSuites proves --distro + --suite narrows the
// walk to one suite subtree within the distro.
func TestGcScopedToSuiteIgnoresOtherSuites(t *testing.T) {
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

	code, stdout, stderr := runGc(t, tmpState, tmpPublic, "--distro", "debian", "--suite", "bookworm")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "ghost-book", "in-scope suite orphan must be removed")
	assert.NotContains(t, stdout, "ghost-bull", "out-of-scope suite orphan must be ignored")

	assert.NoFileExists(t, bookOrphan, "in-scope suite orphan must be removed")
	assert.FileExists(t, bullOrphan, "out-of-scope suite orphan must survive")
}

func TestGcDryRunLeavesFilesOnDisk(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runGc(t, tmpState, tmpPublic, "--dry-run")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "would remove debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.Contains(t, stdout, "gc: would remove 1 orphan(s)")
	assert.NotContains(t, stdout, "gc: removed", "dry-run must not claim a removal")

	assert.FileExists(t, orphan, "--dry-run must not delete anything")
}

func TestGcEmptyPoolTreePasses(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, stdout, stderr := runGc(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "gc: removed 0 orphan(s)")
}

// TestGcPreservesPoolWriteArtifacts guards against gc destroying the
// recoverable copy after an interrupted pool.Replace. A crashed Replace can
// leave dest.bak (old bytes) and dest.tmp (new bytes) while the DB row still
// references dest; classifying these as orphans would turn a recoverable
// state into permanent data loss.
func TestGcPreservesPoolWriteArtifacts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	dest := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(dest+".bak", []byte("old bytes"), 0o644))
	require.NoError(t, os.WriteFile(dest+".tmp", []byte("new bytes"), 0o644))

	code, stdout, stderr := runGc(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.NotContains(t, stdout, ".bak", "pool-write .bak must not be treated as orphan")
	assert.NotContains(t, stdout, ".tmp", "pool-write .tmp must not be treated as orphan")
	assert.Contains(t, stdout, "gc: removed 0 orphan(s)")
	assert.FileExists(t, dest+".bak", "gc must preserve .bak so an interrupted Replace stays recoverable")
	assert.FileExists(t, dest+".tmp", "gc must preserve .tmp so an interrupted Replace stays recoverable")
}

// TestGcQuietSuppressesSummaryNotOrphanLines pins the documented --quiet
// contract: the trailing "gc: removed N orphan(s)" aggregate is suppressed,
// but the per-orphan "removed <path>" lines still print so a quiet run stays
// diagnosable from a script.
func TestGcQuietSuppressesSummaryNotOrphanLines(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "--quiet", "gc",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "removed debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb",
		"--quiet must still print per-orphan lines")
	assert.NotContains(t, stdout.String(), "gc: removed",
		"--quiet must suppress the trailing summary line")
	assert.NoFileExists(t, orphan, "gc must still remove the orphan under --quiet")
}

func TestGcSuiteWithoutDistroIsUsageError(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	code, _, stderr := runGc(t, tmpState, tmpPublic, "--suite", "bookworm")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr, "--suite requires --distro")
}

func TestGcRejectsUnsafeScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	sentinel := filepath.Join(tmpPublic, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("must survive"), 0o644))

	code, _, stderr := runGc(t, tmpState, tmpPublic, "--distro", "..")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr, "--distro")
	assert.FileExists(t, sentinel, "sentinel outside pool/ must not be touched by gc")
}

func TestGcUnconfiguredDistroAborts(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, _, stderr := runGc(t, tmpState, tmpPublic, "--distro", "raspbian")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr, "raspbian")
	assert.Contains(t, stderr, "config.distros")
	assert.FileExists(t, orphan, "gc must abort before any pool walk when --distro is unconfigured")
}

// TestGcPrunesNamespacedDistro guards the namespaced-distro arm of the prune
// path: `customer/acme/pool/...` must anchor its stop boundary at
// <publicDir>/customer/acme/pool, not <publicDir>/customer/pool.
func TestGcPrunesNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")

	orphan := filepath.Join(tmpPublic, "customer", "acme", "pool", "stable", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runGc(t, tmpState, tmpPublic, "--distro", "customer/acme")
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "removed customer/acme/pool/stable/main/g/ghost/ghost_1.0_amd64.deb")

	assert.NoFileExists(t, orphan)
	assert.NoDirExists(t, filepath.Join(tmpPublic, "customer", "acme", "pool", "stable"),
		"emptied suite subtree under namespaced distro must be pruned")
	assert.DirExists(t, filepath.Join(tmpPublic, "customer", "acme", "pool"),
		"namespaced pool root must be preserved (stop boundary at customer/acme/pool)")
	assert.NotContains(t, stderr, "prune:", "no prune warning expected on the happy path")
}

// TestGcLeavesUnconfiguredDistroAlone proves an unrelated top-level tree under
// the public dir (managed by another tool) is invisible to gc when it is not
// declared in config.distros.
func TestGcLeavesUnconfiguredDistroAlone(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	foreign := filepath.Join(tmpPublic, "raspbian", "pool", "bullseye", "main", "f", "foreign",
		"foreign_1.0_armhf.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0o755))
	require.NoError(t, os.WriteFile(foreign, []byte("not repokit's"), 0o644))

	code, stdout, stderr := runGc(t, tmpState, tmpPublic)
	require.Equal(t, exitOK, code, "stderr: %s", stderr)
	assert.NotContains(t, stdout, "raspbian", "gc must never walk unconfigured trees")
	assert.Contains(t, stdout, "gc: removed 0 orphan(s)")

	assert.FileExists(t, foreign, "gc must not touch unconfigured distros under publicDir")
}

// TestGcSerializesWithConcurrentWriter guards the race where an in-flight
// `add` writes its pool bytes inside its own writer transaction but gc,
// computing orphans against committed rows only, deletes the file before that
// transaction commits. Holding a writer lock from a separate connection
// (mimicking add's INSERT-to-COMMIT window) must block gc until release.
func TestGcSerializesWithConcurrentWriter(t *testing.T) {
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

	gcDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		code, _, stderr := runGc(t, tmpState, tmpPublic)
		gcDone <- struct {
			code   int
			stderr string
		}{code, stderr}
	}()

	select {
	case res := <-gcDone:
		t.Fatalf("gc completed while writer lock was held: code=%d stderr=%s", res.code, res.stderr)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	wg.Wait()

	select {
	case res := <-gcDone:
		require.Equal(t, exitOK, res.code, "stderr: %s", res.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("gc did not finish after writer lock release")
	}
}
