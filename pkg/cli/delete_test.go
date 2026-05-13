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

// addFixtureForTest adds a fixture .deb under distro=debian to the given
// suite using the add subcommand so delete tests start from a realistic
// on-disk state. Pool I/O lands under public; the DB lives under state.
func addFixtureForTest(t *testing.T, state, public, suite, fixture string) {
	t.Helper()
	args := []string{
		"--repo", state, "--public", public, "add",
		"--distro", "debian", "--suite", suite,
		fixtureDeb(t, fixture),
	}
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "fixture add failed: %s", stderr.String())
}

// runDelete invokes the delete subcommand with the supplied flags after the
// global --repo and --public flags. Returns exit code, stdout, stderr.
func runDelete(t *testing.T, state, public string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"--repo", state, "--public", public, "delete"}, args...)
	var stdout, stderr bytes.Buffer
	code := Run(full, &stdout, &stderr, "test-rev")
	return code, stdout.String(), stderr.String()
}

// poolFileCount returns the number of regular files under the public dir's
// per-distro pool/ trees.
func poolFileCount(t *testing.T, public string) int {
	t.Helper()
	return len(listPoolFiles(t, public))
}

// rowSHA256s returns the sha256 of every row in the DB, sorted.
func rowSHA256s(t *testing.T, state string) []string {
	t.Helper()
	r, err := repo.Open(context.Background(), filepath.Join(state, dbFilename))
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
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	require.Equal(t, 2, rowCount(t, tmpState))
	require.Equal(t, 2, poolFileCount(t, tmpPublic))

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted hello-gz_1.0_amd64")
	assert.Contains(t, stdout, "deleted 1 package(s)")
	assert.Contains(t, stdout, "run 'repokit publish --distro debian --suite bookworm'")

	assert.Equal(t, 1, rowCount(t, tmpState))
	assert.Equal(t, 1, poolFileCount(t, tmpPublic))
	assert.NoFileExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb"))
	assert.FileExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello",
		"hello-xz_1.0_amd64.deb"))
}

func TestDeleteBySHA256(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	r, err := repo.Open(context.Background(), filepath.Join(tmpState, dbFilename))
	require.NoError(t, err)
	rows, err := r.List(context.Background(), repo.Filter{Name: "hello-gz"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	target := rows[0].SHA256
	require.NoError(t, r.Close())

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--sha256", target,
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 1 package(s)")

	remaining := rowSHA256s(t, tmpState)
	require.Len(t, remaining, 1)
	assert.NotEqual(t, target, remaining[0])
}

func TestDeleteAllVersions(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-xz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz",
	)
	require.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "--all-versions")
	assert.Equal(t, 2, rowCount(t, tmpState), "no rows should be removed when validation fails")

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--all-versions",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 1 package(s)")
	assert.Equal(t, 1, rowCount(t, tmpState))
}

func TestDeleteDryRunMakesNoChanges(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0", "--dry-run",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "would delete hello-gz_1.0_amd64")
	assert.Contains(t, stdout, "(dry run, no changes made)")
	assert.NotContains(t, stdout, "run 'repokit publish")

	assert.Equal(t, 1, rowCount(t, tmpState), "dry-run must leave DB untouched")
	assert.Equal(t, 1, poolFileCount(t, tmpPublic), "dry-run must leave pool untouched")
}

func TestDeleteRequiresFilter(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
	)
	require.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "at least one of --name or --sha256")

	assert.Equal(t, 1, rowCount(t, tmpState))
	assert.Equal(t, 1, poolFileCount(t, tmpPublic))
}

func TestDeleteRequiresDistroAndSuite(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--name", "hello-gz", "--version", "1.0",
	)
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr, "required")
	assert.Equal(t, 1, rowCount(t, tmpState))
}

func TestDeleteNoMatchIsNotAnError(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "ghost", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 0 package(s)")
	assert.Equal(t, 1, rowCount(t, tmpState))
}

func TestDeleteScopedToSuite(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")

	require.Equal(t, 2, rowCount(t, tmpState))
	require.Equal(t, 2, poolFileCount(t, tmpPublic))

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)

	assert.Equal(t, 1, rowCount(t, tmpState))
	assert.Equal(t, 1, poolFileCount(t, tmpPublic))
	assert.NoFileExists(t, filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb"))
	assert.FileExists(t, filepath.Join(tmpPublic, "debian", "pool", "bullseye", "main", "h", "hello-gz",
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

	deleteDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		code, _, stderr := runDelete(t, tmpState, tmpPublic,
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

	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Equal(t, 0, poolFileCount(t, tmpPublic))
}

// TestDeleteRejectsEmptyDistroSuite ensures the safety guard that requires
// --distro and --suite cannot be bypassed by passing them as empty strings.
// Without this, `delete --distro="" --suite="" --name foo --all-versions`
// would translate to a SQL `WHERE name='foo'` and remove rows across every
// distro/suite — a single shell-quoting accident wiping the whole repo.
func TestDeleteRejectsEmptyDistroSuite(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")
	addFixtureForTest(t, tmpState, tmpPublic, "bullseye", "hello-gz_1.0_amd64.deb")

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

			code, _, stderr := runDelete(t, tmpState, tmpPublic, args...)
			assert.Equal(t, ExitUsage, code)
			assert.Contains(t, stderr, "must not be empty")
			assert.Equal(t, 2, rowCount(t, tmpState), "no rows must be removed")
		})
	}
}

// TestDeleteRejectsUnsafeComponentArch guards the validateScopeName gate on
// --component and --arch. Without it, a value like "../etc" would flow into
// the SQL filter and (worse) into any later filesystem operation that joins
// component or arch into a path; a newline would let a Release header be
// forged via the audit log. The test asserts ExitUsage and that no rows are
// removed.
func TestDeleteRejectsUnsafeComponentArch(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	cases := []struct {
		name  string
		field string
		val   string
	}{
		{"component-traversal", "--component", "../etc"},
		{"component-newline", "--component", "main\nLabel: Forged"},
		{"component-leading-dot", "--component", ".hidden"},
		{"arch-traversal", "--arch", "../etc"},
		{"arch-newline", "--arch", "amd64\nLabel: Forged"},
		{"arch-leading-dash", "--arch", "-amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				"--distro", "debian", "--suite", "bookworm",
				tc.field, tc.val,
				"--name", "hello-gz", "--all-versions",
			}
			code, _, stderr := runDelete(t, tmpState, tmpPublic, args...)
			assert.Equal(t, ExitUsage, code)
			assert.Contains(t, stderr, tc.field, "error should name the rejected flag")
			assert.Equal(t, 1, rowCount(t, tmpState), "no rows must be removed")
		})
	}
}

// TestDeleteRejectsUnconfiguredDistro asserts that a --distro value not in
// config.distros aborts delete with ExitError before any DB row is touched.
// The fixture distro list is debian/ubuntu (from initRepoForTest); "raspbian"
// is deliberately absent.
func TestDeleteRejectsUnconfiguredDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "raspbian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr, `"raspbian"`,
		"error should name the offending --distro value")
	assert.Contains(t, stderr, "config.distros",
		"error should mention the config field the operator must edit")

	assert.Equal(t, 1, rowCount(t, tmpState), "no DB row must be removed")
	assert.Equal(t, 1, poolFileCount(t, tmpPublic), "no pool file must be removed")
}

// TestDeleteAcceptsNamespacedDistro covers the Task 3 repointing of the
// --distro validator on delete. With distros: [customer/acme] declared, a
// `delete --distro customer/acme` invocation must pass requireDistroScope
// (no slash rejection) and requireConfiguredDistro (lookup matches).
func TestDeleteAcceptsNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")

	insertPackage(t, tmpState, repo.Package{
		Distro:       "customer/acme",
		Suite:        "stable",
		Component:    "main",
		Architecture: "amd64",
		Name:         "foo",
		Version:      "1.0",
		Filename:     "pool/stable/main/f/foo/foo_1.0_amd64.deb",
		Size:         1,
		MD5:          "00000000000000000000000000000000",
		SHA1:         "0000000000000000000000000000000000000000",
		SHA256:       "0000000000000000000000000000000000000000000000000000000000000000",
		SHA512: "00000000000000000000000000000000000000000000000000000000000000000" +
			"0000000000000000000000000000000000000000000000000000000000000000",
		Control: "Package: foo\nVersion: 1.0\nArchitecture: amd64\n",
	})

	code, stdout, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "customer/acme", "--suite", "stable",
		"--name", "foo", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "deleted 1 package(s)")
	assert.Equal(t, 0, rowCount(t, tmpState))
}

// TestDeletePoolUnlinksTargetPublicDir is a focused regression check that the
// pool unlink path uses ctx.PublicDir, not ctx.Root: the state dir must be
// untouched by the delete, and the file under tmpPublic must be gone.
func TestDeletePoolUnlinksTargetPublicDir(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	addFixtureForTest(t, tmpState, tmpPublic, "bookworm", "hello-gz_1.0_amd64.deb")

	poolFile := filepath.Join(tmpPublic, "debian", "pool", "bookworm", "main", "h", "hello-gz",
		"hello-gz_1.0_amd64.deb")
	require.FileExists(t, poolFile)

	statePre, err := os.ReadDir(tmpState)
	require.NoError(t, err)

	code, _, stderr := runDelete(t, tmpState, tmpPublic,
		"--distro", "debian", "--suite", "bookworm",
		"--name", "hello-gz", "--version", "1.0",
	)
	require.Equal(t, ExitOK, code, "stderr: %s", stderr)

	assert.NoFileExists(t, poolFile, "pool file under public dir must be unlinked")
	assert.Empty(t, listPoolFiles(t, tmpState), "no .deb should ever land in the state dir")

	statePost, err := os.ReadDir(tmpState)
	require.NoError(t, err)
	prePostMatch := func(a, b []os.DirEntry) bool {
		if len(a) != len(b) {
			return false
		}
		seen := make(map[string]bool, len(a))
		for _, e := range a {
			seen[e.Name()] = true
		}
		for _, e := range b {
			if !seen[e.Name()] {
				return false
			}
		}
		return true
	}
	assert.True(t, prePostMatch(statePre, statePost),
		"state dir top-level entries must not change across a delete run")

	assert.Equal(t, 0, rowCount(t, tmpState), "DB row should be gone after delete")
}
