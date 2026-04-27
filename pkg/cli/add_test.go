package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alkk/repokit/pkg/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRepoForTest creates a fresh repo at root using the init subcommand.
func initRepoForTest(t *testing.T, root string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "init failed: %s", stderr.String())
}

// fixtureDeb returns the absolute path to a testdata .deb fixture sitting in
// pkg/deb/testdata/. Tests run with cwd == pkg/cli, so we resolve relative.
func fixtureDeb(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "deb", "testdata", name))
	require.NoError(t, err)
	require.FileExists(t, abs)
	return abs
}

// fileSHA256 computes the sha256 of path as a lower-case hex string.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// listPoolFiles returns every .deb file beneath any per-distro pool tree as a
// slice of repo-root-relative paths (forward-slashed). Walks the whole root so
// it catches files under <root>/<distro>/pool/... regardless of distro.
func listPoolFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".deb") {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr //nolint:wrapcheck // surfaces verbatim to require.NoError below
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	return out
}

// rowCount returns the number of rows visible in the packages table.
func rowCount(t *testing.T, root string) int {
	t.Helper()
	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	defer r.Close()
	rows, err := r.List(context.Background(), repo.Filter{})
	require.NoError(t, err)
	return len(rows)
}

func TestAddSinglePackage(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	wantFilename := "pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"
	wantDisk := "debian/" + wantFilename
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(wantDisk)))
	assert.Equal(t, fileSHA256(t, deb), fileSHA256(t, filepath.Join(root, filepath.FromSlash(wantDisk))),
		"pool file should be a byte-identical copy of the source")

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	rows, err := r.List(context.Background(), repo.Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "debian", got.Distro)
	assert.Equal(t, "bookworm", got.Suite)
	assert.Equal(t, "main", got.Component)
	assert.Equal(t, "amd64", got.Architecture)
	assert.Equal(t, "hello-gz", got.Name)
	assert.Equal(t, "1.0", got.Version)
	assert.Equal(t, wantFilename, got.Filename)
	assert.Equal(t, fileSHA256(t, deb), got.SHA256)
	assert.Contains(t, got.Control, "Package: hello-gz")
	assert.True(t, strings.HasSuffix(got.Control, "\n"), "stored control must end with a single newline")

	assert.Contains(t, stdout.String(), "staged 1 packages in debian/bookworm")
}

func TestAddMultiplePackages(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	debs := []string{
		fixtureDeb(t, "hello-gz_1.0_amd64.deb"),
		fixtureDeb(t, "hello-xz_1.0_amd64.deb"),
		fixtureDeb(t, "hello-zst_1.0_amd64.deb"),
	}

	args := append([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
	}, debs...)
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	pool := listPoolFiles(t, root)
	assert.ElementsMatch(t, []string{
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb",
		"debian/pool/bookworm/main/h/hello/hello-xz_1.0_amd64.deb",
		"debian/pool/bookworm/main/f/foo/hello-zst_1.0_amd64.deb",
	}, pool)

	assert.Equal(t, 3, rowCount(t, root))
	assert.Contains(t, stdout.String(), "staged 3 packages")
}

func TestAddDuplicateWithoutReplace(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, root, deb)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "duplicate hello-gz_1.0_amd64")
	assert.Contains(t, stderr.String(), "existing sha256:")
	assert.Contains(t, stderr.String(), "new sha256:")
	assert.Contains(t, stderr.String(), "use --replace")

	assert.Equal(t, 1, rowCount(t, root), "DB row count should be unchanged after duplicate add")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")))
}

func TestAddReplaceOverwrites(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	realSHA := fileSHA256(t, deb)

	// Seed a row whose hashes do not match the .deb on disk so --replace must
	// actually overwrite for the post-condition to hold. Re-adding the same
	// fixture would tautologically pass even if --replace silently became a no-op.
	const placeholderSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	filename := "pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"
	pre := filepath.Join(root, "debian", filepath.FromSlash(filename))
	require.NoError(t, os.MkdirAll(filepath.Dir(pre), 0o755))
	require.NoError(t, os.WriteFile(pre, []byte("stale-bytes"), 0o644))
	insertPackage(t, root, repo.Package{
		Distro:       "debian",
		Suite:        "bookworm",
		Component:    "main",
		Architecture: "amd64",
		Name:         "hello-gz",
		Version:      "1.0",
		Filename:     filename,
		Size:         11,
		MD5:          "00000000000000000000000000000000",
		SHA1:         "0000000000000000000000000000000000000000",
		SHA256:       placeholderSHA256,
		SHA512:       strings.Repeat("0", 128),
		Control:      "Package: hello-gz\nVersion: 1.0\nArchitecture: amd64\n",
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	assert.Equal(t, 1, rowCount(t, root), "replace should keep exactly one row")

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	rows, err := r.List(context.Background(), repo.Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.NotEqual(t, placeholderSHA256, rows[0].SHA256, "--replace must update the placeholder SHA256")
	assert.Equal(t, realSHA, rows[0].SHA256, "--replace must set the SHA256 to the real .deb hash")
	assert.Equal(t, realSHA, fileSHA256(t, pre), "--replace must overwrite the pool file with the real .deb bytes")
}

func TestAddRequiresFlags(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr.String(), "required")

	assert.Equal(t, 0, rowCount(t, root))
	assert.Empty(t, listPoolFiles(t, root))
}

func TestAddBadDebFails(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	bogus := filepath.Join(t.TempDir(), "bogus.deb")
	require.NoError(t, os.WriteFile(bogus, []byte("not a real .deb"), 0o600))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
		bogus,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "add:")

	assert.Equal(t, 0, rowCount(t, root))
	assert.Empty(t, listPoolFiles(t, root))
}

func TestAddRejectsMultiDebReplace(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb1 := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	deb2 := fixtureDeb(t, "hello-xz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb1, deb2,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "single .deb argument")
	assert.Equal(t, 0, rowCount(t, root))
}

func TestAddRejectsUnsafeScope(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "../etc", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "--distro")
	assert.Equal(t, 0, rowCount(t, root))
	assert.Empty(t, listPoolFiles(t, root))
}

// TestAddRejectsEmptyScope guards against a typo or shell-quoting accident
// (e.g. `--distro="$DISTRO"` with DISTRO unset) silently producing a malformed
// pool layout. go-flags' required:"true" only checks flag presence, so we
// reject empty values explicitly.
func TestAddRejectsEmptyScope(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	for _, field := range []string{"--distro", "--suite", "--component"} {
		t.Run(field, func(t *testing.T) {
			args := []string{"--repo", root, "add"}
			switch field {
			case "--distro":
				args = append(args, "--distro", "", "--suite", "bookworm")
			case "--suite":
				args = append(args, "--distro", "debian", "--suite", "")
			case "--component":
				args = append(args, "--distro", "debian", "--suite", "bookworm", "--component", "")
			}
			args = append(args, deb)

			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr, "test-rev")
			assert.Equal(t, ExitError, code)
			assert.Contains(t, stderr.String(), "must not be empty")
			assert.Equal(t, 0, rowCount(t, root))
			assert.Empty(t, listPoolFiles(t, root))
		})
	}
}

// TestAddRejectsControlCharsInScope ensures a Release header injection via
// embedded newlines in --suite/--distro is blocked at the CLI layer.
func TestAddRejectsControlCharsInScope(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm\nLabel: Forged",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "control characters")
	assert.Equal(t, 0, rowCount(t, root))
	assert.Empty(t, listPoolFiles(t, root))
}

// TestAddBatchRollback covers the rollback semantics for a batch where
// earlier items succeed before a later item fails. The expected final state
// is no DB rows and no pool files.
func TestAddBatchRollback(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	good1 := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	good2 := fixtureDeb(t, "hello-xz_1.0_amd64.deb")
	missing := filepath.Join(t.TempDir(), "definitely-missing.deb")

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
		good1, good2, missing,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)

	assert.Equal(t, 0, rowCount(t, root), "DB must be empty after batch rollback")
	assert.Empty(t, listPoolFiles(t, root), "pool must contain no files after batch rollback")

	// Sanity: the parent directories that pool.Write created are allowed to
	// remain — only files matter for cleanup.
}

// TestAddReplaceSweepsOldPoolFileOnSourceChange covers the case where an
// existing row's pool path differs from the new --replace target (the .deb's
// Source field changed between versions, moving the shard). The old physical
// file at the prior shard must be swept rather than left as a silent orphan.
func TestAddReplaceSweepsOldPoolFileOnSourceChange(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-zst_1.0_amd64.deb") // Source: foo → shard f/foo

	stalePool := filepath.Join(root, filepath.FromSlash(
		"debian/pool/bookworm/main/w/wrong-source/hello-zst_1.0_amd64.deb"))
	require.NoError(t, os.MkdirAll(filepath.Dir(stalePool), 0o755))
	require.NoError(t, os.WriteFile(stalePool, []byte("stale-shard"), 0o644))

	insertPackage(t, root, repo.Package{
		Distro:       "debian",
		Suite:        "bookworm",
		Component:    "main",
		Architecture: "amd64",
		Name:         "hello-zst",
		Version:      "1.0",
		Filename:     "pool/bookworm/main/w/wrong-source/hello-zst_1.0_amd64.deb",
		Size:         11,
		MD5:          "00000000000000000000000000000000",
		SHA1:         "0000000000000000000000000000000000000000",
		SHA256:       "0000000000000000000000000000000000000000000000000000000000000000",
		SHA512:       strings.Repeat("0", 128),
		Control:      "Package: hello-zst\nVersion: 1.0\nArchitecture: amd64\n",
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	assert.NoFileExists(t, stalePool, "old pool file at prior shard must be removed")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(
		"debian/pool/bookworm/main/f/foo/hello-zst_1.0_amd64.deb")),
		"new pool file must exist at the canonical Source-derived shard")
	assert.Equal(t, 1, rowCount(t, root), "DB must hold exactly one row after replace")
}

// TestAddReplaceClearsStaleBackup covers the crash-after-commit recovery:
// a prior --replace successfully committed but crashed before
// FinalizeReplace ran, leaving a stale .bak. The next --replace must
// recognize that dest is canonical (the DB row references it) and remove
// the .bak before its own stashExisting runs — otherwise that stashExisting
// would adopt the stale .bak as recovery material and a later rollback
// could restore bytes from two versions ago.
func TestAddReplaceClearsStaleBackup(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, root, deb)

	dest := filepath.Join(root, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
	require.NoError(t, os.WriteFile(dest+".bak", []byte("stale-prev-version"), 0o644))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	assert.NoFileExists(t, dest+".bak",
		"reconciled stale .bak plus the new .bak from this run must both be cleaned up")
	assert.FileExists(t, dest, "dest must hold the new bytes after --replace")
	assert.Equal(t, fileSHA256(t, deb), fileSHA256(t, dest),
		"dest must be a byte-identical copy of the .deb that was just added")
}

// TestAddReplaceRestoresUncommittedDestAfterCrash covers the
// crash-before-commit recovery: a prior --replace promoted new bytes to
// dest then crashed before its surrounding Tx committed. SQLite rolled
// back, leaving the prior row in place — its Filename still references
// this same relPath because the NVRA is unchanged. dest now holds the
// uncommitted bytes from the crashed run while .bak holds the bytes that
// match the rolled-back row. The next --replace must verify dest's SHA256
// against the row before treating dest as canonical: trusting Filename
// alone would silently destroy the only canonical copy (.bak) and adopt
// the uncommitted dest as recovery material on a later rollback.
func TestAddReplaceRestoresUncommittedDestAfterCrash(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, root, deb)
	realSHA := fileSHA256(t, deb)

	dest := filepath.Join(root, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
	require.NoError(t, os.Rename(dest, dest+".bak"))
	require.NoError(t, os.WriteFile(dest, []byte("uncommitted-from-crashed-replace"), 0o644))

	cmd := &AddCmd{
		Distro:    "debian",
		Suite:     "bookworm",
		Component: "main",
		Replace:   true,
	}
	ctx := context.Background()
	r, err := repo.Open(ctx, filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	c := &Context{Root: root, Stdout: io.Discard, Stderr: io.Discard}
	require.NoError(t, cmd.reconcileBackup(ctx, c, r, deb))

	assert.NoFileExists(t, dest+".bak", ".bak must be moved over dest after reconcile")
	assert.Equal(t, realSHA, fileSHA256(t, dest),
		"dest must hold the canonical bytes after reconcile (restored from .bak)")
}

// addOnce performs a baseline successful add of a single .deb so subsequent
// scenarios can exercise duplicate / replace logic against a populated repo.
func addOnce(t *testing.T, root, deb string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "baseline add failed: %s", stderr.String())
}
