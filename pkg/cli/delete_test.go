package cli

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/netxms/debrepo/pkg/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addFixtureForTest adds a fixture .deb under distro=debian to the given
// suite using the add subcommand so delete tests start from a realistic
// on-disk state.
func addFixtureForTest(t *testing.T, root, suite, fixture string) {
	t.Helper()
	args := []string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", suite,
		fixtureDeb(t, fixture),
	}
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "fixture add failed: %s", stderr.String())
}

// runDelete invokes the delete subcommand with the supplied flags after the
// global --repo flag. Returns exit code, stdout, stderr.
func runDelete(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", root, "delete"}, args...)
	var stdout, stderr bytes.Buffer
	code := Run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

// poolFileCount returns the number of regular files under pool/.
func poolFileCount(t *testing.T, root string) int {
	t.Helper()
	return len(listPoolFiles(t, root))
}

// rowSHA256s returns the sha256 of every row in the DB, sorted.
func rowSHA256s(t *testing.T, root string) []string {
	t.Helper()
	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	defer r.Close()
	rows, err := r.List(context.Background(), repo.Filter{})
	require.NoError(t, err)
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.SHA256)
	}
	return out
}

func TestDeleteByNameAndVersion(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bookworm", "hello-xz_1.0_amd64.deb")

	require.Equal(t, 2, rowCount(t, root))
	require.Equal(t, 2, poolFileCount(t, root))

	code, stdout, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted hello-gz_1.0_amd64")
	assert.Contains(t, stdout, "deleted 1 package(s)")
	assert.Contains(t, stdout, "run 'debrepo publish --distro debian --suite bookworm'")

	assert.Equal(t, 1, rowCount(t, root))
	assert.Equal(t, 1, poolFileCount(t, root))
	assert.NoFileExists(t, filepath.Join(root, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb"))
	assert.FileExists(t, filepath.Join(root, "debian", "pool", "bookworm", "main", "h", "hello",
		"hello-xz_1.0_amd64.deb"))
}

func TestDeleteBySHA256(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bookworm", "hello-xz_1.0_amd64.deb")

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	rows, err := r.List(context.Background(), repo.Filter{Name: "hello-gz"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	target := rows[0].SHA256
	require.NoError(t, r.Close())

	code, stdout, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--sha256", target,
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 1 package(s)")

	remaining := rowSHA256s(t, root)
	require.Len(t, remaining, 1)
	assert.NotEqual(t, target, remaining[0])
}

func TestDeleteAllVersions(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bookworm", "hello-xz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz",
	)
	require.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "--all-versions")
	assert.Equal(t, 2, rowCount(t, root), "no rows should be removed when validation fails")

	code, stdout, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--all-versions",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 1 package(s)")
	assert.Equal(t, 1, rowCount(t, root))
}

func TestDeleteDryRunMakesNoChanges(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	code, stdout, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0", "--dry-run",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "would delete hello-gz_1.0_amd64")
	assert.Contains(t, stdout, "(dry run, no changes made)")
	assert.NotContains(t, stdout, "run 'debrepo publish")

	assert.Equal(t, 1, rowCount(t, root), "dry-run must leave DB untouched")
	assert.Equal(t, 1, poolFileCount(t, root), "dry-run must leave pool untouched")
}

func TestDeleteRequiresFilter(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
	)
	require.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "at least one of --name or --sha256")

	assert.Equal(t, 1, rowCount(t, root))
	assert.Equal(t, 1, poolFileCount(t, root))
}

func TestDeleteRequiresDistroAndSuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, root,
		"--name", "hello-gz", "--version", "1.0",
	)
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "required")
	assert.Equal(t, 1, rowCount(t, root))
}

func TestDeleteNoMatchIsNotAnError(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")

	code, stdout, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "ghost", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 0 package(s)")
	assert.Equal(t, 1, rowCount(t, root))
}

func TestDeleteScopedToSuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bullseye", "hello-gz_1.0_amd64.deb")

	require.Equal(t, 2, rowCount(t, root))
	require.Equal(t, 2, poolFileCount(t, root))

	code, _, stderr := runDelete(t, root,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)

	assert.Equal(t, 1, rowCount(t, root))
	assert.Equal(t, 1, poolFileCount(t, root))
	assert.NoFileExists(t, filepath.Join(root, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb"))
	assert.FileExists(t, filepath.Join(root, "debian", "pool", "bullseye", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb"))
}

// TestDeleteSerializesWithConcurrentWriter guards the race where delete
// commits its row removal and then unlinks pool files outside any lock: a
// concurrent `add --replace` for the same NVRA could insert a fresh row
// pointing at the same filename and write the new bytes into the dest
// before delete's pool.Delete loop runs, silently destroying the just-
// written bytes. The fix wraps the whole delete (rows + pool unlinks) in a
// writer lock; this test holds that lock from another connection and
// asserts delete blocks until release.
func TestDeleteSerializesWithConcurrentWriter(t *testing.T) {
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

	deleteDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		code, _, stderr := runDelete(t, root,
			"--distro", "debian", "--suite", "bookworm",
			"--name", "hello-gz", "--version", "1.0",
		)
		deleteDone <- struct {
			code   int
			stderr string
		}{code, stderr}
	}()

	select {
	case res := <-deleteDone:
		t.Fatalf("delete completed while writer lock was held: code=%d stderr=%s", res.code, res.stderr)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	wg.Wait()

	select {
	case res := <-deleteDone:
		require.Equal(t, ExitOK, res.code, "stderr: %s", res.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not finish after writer lock release")
	}

	assert.Equal(t, 0, rowCount(t, root))
	assert.Equal(t, 0, poolFileCount(t, root))
}

// TestDeleteRejectsEmptyDistroSuite ensures the safety guard that requires
// --distro and --suite cannot be bypassed by passing them as empty strings.
// Without this, `delete --distro="" --suite="" --name foo --all-versions`
// would translate to a SQL `WHERE name='foo'` and remove rows across every
// distro/suite — a single shell-quoting accident wiping the whole repo.
func TestDeleteRejectsEmptyDistroSuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	addFixtureForTest(t, root, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, root, "bullseye", "hello-gz_1.0_amd64.deb")

	for _, field := range []string{"--distro", "--suite"} {
		t.Run(field, func(t *testing.T) {
			args := []string{}
			switch field {
			case "--distro":
				args = []string{"--distro", "", "--suite", "bookworm",
					"--name", "hello-gz", "--all-versions"}
			case "--suite":
				args = []string{"--distro", "debian", "--suite", "",
					"--name", "hello-gz", "--all-versions"}
			}

			code, _, stderr := runDelete(t, root, args...)
			assert.Equal(t, ExitUsage, code)
			assert.Contains(t, stderr, "must not be empty")
			assert.Equal(t, 2, rowCount(t, root), "no rows must be removed")
		})
	}
}
