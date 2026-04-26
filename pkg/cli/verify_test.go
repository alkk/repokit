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

	"github.com/netxms/debrepo/pkg/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runVerify invokes the verify subcommand with the supplied flags after the
// global --repo flag. Returns exit code, stdout, stderr.
func runVerify(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", root, "verify"}, args...)
	var stdout, stderr bytes.Buffer
	code := Run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

func TestVerifyCleanRepoPasses(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bookworm", "hello-xz_1.0_amd64.deb")

	code, stdout, stderr := runVerify(t, root)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "mismatch")
	assert.NotContains(t, stdout, "orphan")
	assert.NotContains(t, stdout, "missing")
}

func TestVerifyTamperedFileDetected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	tampered := filepath.Join(root, "pool", "debian", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(tampered, []byte("not a real deb"), 0o644))

	code, stdout, stderr := runVerify(t, root)
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "mismatch pool/debian/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 mismatch(es)")
}

func TestVerifyMissingPoolFileDetected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(root, "pool", "debian", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runVerify(t, root)
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing pool/debian/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 missing")
}

func TestVerifyOrphanDetected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	orphan := filepath.Join(root, "pool", "debian", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, root)
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan pool/debian/bookworm/main/g/ghost/ghost_1.0_amd64.deb")
	assert.Contains(t, stdout, "1 orphan(s)")

	assert.FileExists(t, orphan, "orphan must remain on disk without --gc")
}

func TestVerifyGCRemovesOnlyOrphan(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	legit := filepath.Join(root, "pool", "debian", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	orphan := filepath.Join(root, "pool", "debian", "bookworm", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(orphan), 0o755))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, root, "--gc")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "orphan pool/debian/bookworm/main/g/ghost/ghost_1.0_amd64.deb (removed)")
	assert.Contains(t, stdout, "verify: ok")

	assert.NoFileExists(t, orphan, "--gc must remove orphan")
	assert.FileExists(t, legit, "--gc must leave legit pool files in place")
	assert.Equal(t, 1, rowCount(t, root), "--gc must not touch DB rows")
}

func TestVerifyGCDoesNotRemoveRowsForMissingPoolFiles(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	target := filepath.Join(root, "pool", "debian", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.Remove(target))

	code, stdout, stderr := runVerify(t, root, "--gc")
	require.Equal(t, ExitError, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "missing")
	assert.Equal(t, 1, rowCount(t, root), "rows for missing pool files must survive --gc")
}

func TestVerifyScopedToSuiteIgnoresOtherSuites(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bullseye", "hello-gz_1.0_amd64.deb")

	otherOrphan := filepath.Join(root, "pool", "debian", "bullseye", "main", "g", "ghost",
		"ghost_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherOrphan), 0o755))
	require.NoError(t, os.WriteFile(otherOrphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, root, "--distro", "debian", "--suite", "bookworm")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
	assert.NotContains(t, stdout, "ghost", "out-of-scope orphan must not be reported")
	assert.FileExists(t, otherOrphan)
}

func TestVerifySuiteWithoutDistroErrors(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	code, _, stderr := runVerify(t, root, "--suite", "bookworm")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "--suite requires --distro")
}

func TestVerifyEmptyRepoPasses(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	code, stdout, stderr := runVerify(t, root)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "verify: ok")
}

// TestVerifyScopedToDistroCoversAllSuites guards the middle branch of
// scopePoolDir (distro set, suite empty). Without it, a regression that
// collapsed the distro-only case to either the suite-scoped or root path would
// silently include or exclude the wrong subtree of orphans.
func TestVerifyScopedToDistroCoversAllSuites(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bullseye", "hello-gz_1.0_amd64.deb")

	bookOrphan := filepath.Join(root, "pool", "debian", "bookworm", "main", "g", "ghost",
		"ghost-book_1.0_amd64.deb")
	bullOrphan := filepath.Join(root, "pool", "debian", "bullseye", "main", "g", "ghost",
		"ghost-bull_1.0_amd64.deb")
	for _, p := range []string{bookOrphan, bullOrphan} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("orphan"), 0o644))
	}
	otherDistroOrphan := filepath.Join(root, "pool", "ubuntu", "noble", "main", "g", "ghost",
		"ghost-noble_1.0_amd64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherDistroOrphan), 0o755))
	require.NoError(t, os.WriteFile(otherDistroOrphan, []byte("orphan"), 0o644))

	code, stdout, stderr := runVerify(t, root, "--distro", "debian", "--gc")
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
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	dest := filepath.Join(root, "pool", "debian", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.NoError(t, os.WriteFile(dest+".bak", []byte("old bytes"), 0o644))
	require.NoError(t, os.WriteFile(dest+".tmp", []byte("new bytes"), 0o644))

	code, stdout, stderr := runVerify(t, root, "--gc")
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
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
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
		code, _, stderr := runVerify(t, root, "--gc")
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

func TestVerifyRejectsUnsafeScope(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	sentinel := filepath.Join(root, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("must survive"), 0o644))

	code, _, stderr := runVerify(t, root, "--gc", "--distro", "..")
	require.Equal(t, ExitError, code)
	assert.Contains(t, stderr, "--distro")
	assert.FileExists(t, sentinel, "sentinel outside pool/ must not be touched by verify --gc")
}
