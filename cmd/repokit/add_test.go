package main

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

// initRepoForTest creates a fresh repo at root using the init subcommand and
// appends a baseline `distros:` block (debian, ubuntu) to the seed config so
// write subcommands gated by requireConfiguredDistro accept the values the
// existing test suite uses.
func initRepoForTest(t *testing.T, root string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "init failed: %s", stderr.String())
	appendTestDistros(t, root, "debian", "ubuntu")
}

// appendTestDistros rewrites the seed config's `distros:` block with the given
// names so write subcommands accept exactly the values the test wants. Any
// existing `distros:` block (from the init template) is stripped first to keep
// the YAML free of duplicate keys.
func appendTestDistros(t *testing.T, root string, distros ...string) {
	t.Helper()
	cfgPath := filepath.Join(root, "config.yaml")
	body, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	lines := strings.Split(string(body), "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if skipping {
			if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
			skipping = false
		}
		if strings.HasPrefix(line, "distros:") {
			skipping = true
			continue
		}
		out = append(out, line)
	}
	rewritten := strings.Join(out, "\n")

	var b strings.Builder
	b.WriteString(rewritten)
	if rewritten != "" && rewritten[len(rewritten)-1] != '\n' {
		b.WriteString("\n")
	}
	b.WriteString("distros:\n")
	for _, d := range distros {
		b.WriteString("  - ")
		b.WriteString(d)
		b.WriteString("\n")
	}
	require.NoError(t, os.WriteFile(cfgPath, []byte(b.String()), 0o600))
}

// fixtureDeb returns the absolute path to a testdata .deb fixture sitting in
// pkg/deb/testdata/. Tests run with cwd == cmd/repokit, so we resolve relative.
func fixtureDeb(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "pkg", "deb", "testdata", name))
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
// slice of public-root-relative paths (forward-slashed). Walks the whole public
// dir so it catches files under <public>/<distro>/pool/... regardless of distro.
func listPoolFiles(t *testing.T, publicDir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(publicDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".deb") {
			return nil
		}
		rel, rerr := filepath.Rel(publicDir, p)
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
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	wantFilename := "pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"
	wantDisk := "debian/" + wantFilename
	publicPath := filepath.Join(tmpPublic, filepath.FromSlash(wantDisk))
	assert.FileExists(t, publicPath)
	assert.Equal(t, fileSHA256(t, deb), fileSHA256(t, publicPath),
		"pool file should be a byte-identical copy of the source")
	assert.NoFileExists(t, filepath.Join(tmpState, filepath.FromSlash(wantDisk)),
		"pool file must not leak into the state dir")

	r, err := repo.Open(context.Background(), filepath.Join(tmpState, dbFilename))
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
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	debs := []string{
		fixtureDeb(t, "hello-gz_1.0_amd64.deb"),
		fixtureDeb(t, "hello-xz_1.0_amd64.deb"),
		fixtureDeb(t, "hello-zst_1.0_amd64.deb"),
	}

	args := append([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
	}, debs...)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	pool := listPoolFiles(t, tmpPublic)
	assert.ElementsMatch(t, []string{
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb",
		"debian/pool/bookworm/main/h/hello/hello-xz_1.0_amd64.deb",
		"debian/pool/bookworm/main/f/foo/hello-zst_1.0_amd64.deb",
	}, pool)
	assert.Empty(t, listPoolFiles(t, tmpState), "no pool files should land in the state dir")

	assert.Equal(t, 3, rowCount(t, tmpState))
	assert.Contains(t, stdout.String(), "staged 3 packages")
}

func TestAddDuplicateWithoutReplace(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "duplicate hello-gz_1.0_amd64")
	assert.Contains(t, stderr.String(), "existing sha256:")
	assert.Contains(t, stderr.String(), "new sha256:")
	assert.Contains(t, stderr.String(), "use --replace")

	assert.Equal(t, 1, rowCount(t, tmpState), "DB row count should be unchanged after duplicate add")
	assert.FileExists(t, filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")))
}

func TestAddReplaceOverwrites(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	realSHA := fileSHA256(t, deb)

	// Seed a row whose hashes do not match the .deb on disk so --replace must
	// actually overwrite for the post-condition to hold. Re-adding the same
	// fixture would tautologically pass even if --replace silently became a no-op.
	const placeholderSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	filename := "pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"
	pre := filepath.Join(tmpPublic, "debian", filepath.FromSlash(filename))
	require.NoError(t, os.MkdirAll(filepath.Dir(pre), 0o755))
	require.NoError(t, os.WriteFile(pre, []byte("stale-bytes"), 0o644))
	insertPackage(t, tmpState, repo.Package{
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
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	assert.Equal(t, 1, rowCount(t, tmpState), "replace should keep exactly one row")

	r, err := repo.Open(context.Background(), filepath.Join(tmpState, dbFilename))
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
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "required")

	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Empty(t, listPoolFiles(t, tmpPublic))
}

func TestAddBadDebFails(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	bogus := filepath.Join(t.TempDir(), "bogus.deb")
	require.NoError(t, os.WriteFile(bogus, []byte("not a real .deb"), 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
		bogus,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "add:")

	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Empty(t, listPoolFiles(t, tmpPublic))
}

func TestAddRejectsMultiDebReplace(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb1 := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	deb2 := fixtureDeb(t, "hello-xz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb1, deb2,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "single .deb argument")
	assert.Equal(t, 0, rowCount(t, tmpState))
}

func TestAddRejectsUnsafeScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "../etc", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "--distro")
	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Empty(t, listPoolFiles(t, tmpPublic))
}

// TestAddRejectsEmptyScope guards against a typo or shell-quoting accident
// (e.g. `--distro="$DISTRO"` with DISTRO unset) silently producing a malformed
// pool layout. go-flags' required:"true" only checks flag presence, so we
// reject empty values explicitly.
func TestAddRejectsEmptyScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	for _, field := range []string{"--distro", "--suite", "--component"} {
		t.Run(field, func(t *testing.T) {
			args := []string{"--repo", tmpState, "--public", tmpPublic, "add"}
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
			code := run(args, &stdout, &stderr, "test-rev")
			assert.Equal(t, exitError, code)
			assert.Contains(t, stderr.String(), "must not be empty")
			assert.Equal(t, 0, rowCount(t, tmpState))
			assert.Empty(t, listPoolFiles(t, tmpPublic))
		})
	}
}

// TestAddRejectsControlCharsInScope ensures a Release header injection via
// embedded newlines in --suite/--distro is blocked at the CLI layer.
func TestAddRejectsControlCharsInScope(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm\nLabel: Forged",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "control characters")
	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Empty(t, listPoolFiles(t, tmpPublic))
}

// TestAddBatchRollback covers the rollback semantics for a batch where
// earlier items succeed before a later item fails. The expected final state
// is no DB rows and no pool files.
func TestAddBatchRollback(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)

	good1 := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	good2 := fixtureDeb(t, "hello-xz_1.0_amd64.deb")
	missing := filepath.Join(t.TempDir(), "definitely-missing.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
		good1, good2, missing,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)

	assert.Equal(t, 0, rowCount(t, tmpState), "DB must be empty after batch rollback")
	assert.Empty(t, listPoolFiles(t, tmpPublic), "pool must contain no files after batch rollback")

	// Sanity: the parent directories that pool.Write created are allowed to
	// remain — only files matter for cleanup.
}

// TestAddReplaceSweepsOldPoolFileOnSourceChange covers the case where an
// existing row's pool path differs from the new --replace target (the .deb's
// Source field changed between versions, moving the shard). The old physical
// file at the prior shard must be swept rather than left as a silent orphan.
func TestAddReplaceSweepsOldPoolFileOnSourceChange(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-zst_1.0_amd64.deb") // Source: foo → shard f/foo

	stalePool := filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/w/wrong-source/hello-zst_1.0_amd64.deb"))
	require.NoError(t, os.MkdirAll(filepath.Dir(stalePool), 0o755))
	require.NoError(t, os.WriteFile(stalePool, []byte("stale-shard"), 0o644))

	insertPackage(t, tmpState, repo.Package{
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
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	assert.NoFileExists(t, stalePool, "old pool file at prior shard must be removed")
	assert.FileExists(t, filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/f/foo/hello-zst_1.0_amd64.deb")),
		"new pool file must exist at the canonical Source-derived shard")
	assert.Equal(t, 1, rowCount(t, tmpState), "DB must hold exactly one row after replace")
}

// TestAddReplaceClearsStaleBackup covers the crash-after-commit recovery:
// a prior --replace successfully committed but crashed before
// FinalizeReplace ran, leaving a stale .bak. The next --replace must
// recognize that dest is canonical (the DB row references it) and remove
// the .bak before its own stashExisting runs — otherwise that stashExisting
// would adopt the stale .bak as recovery material and a later rollback
// could restore bytes from two versions ago.
func TestAddReplaceClearsStaleBackup(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)

	dest := filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
	require.NoError(t, os.WriteFile(dest+".bak", []byte("stale-prev-version"), 0o644))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

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
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)
	realSHA := fileSHA256(t, deb)

	dest := filepath.Join(tmpPublic, filepath.FromSlash(
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
	r, err := repo.Open(ctx, filepath.Join(tmpState, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	c := &Context{Root: tmpState, PublicDir: tmpPublic, Stdout: io.Discard, Stderr: io.Discard}
	require.NoError(t, cmd.reconcileBackup(ctx, c, r, deb))

	assert.NoFileExists(t, dest+".bak", ".bak must be moved over dest after reconcile")
	assert.Equal(t, realSHA, fileSHA256(t, dest),
		"dest must hold the canonical bytes after reconcile (restored from .bak)")
}

// TestAddPoolWritesGoToPublicDir is a focused regression check that the pool
// I/O paths use ctx.PublicDir, not ctx.Root: the state dir must remain free
// of any per-distro pool tree after a successful add.
func TestAddPoolWritesGoToPublicDir(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)

	assert.Empty(t, listPoolFiles(t, tmpState),
		"no .deb should land in the state dir after an add")
	assert.NotEmpty(t, listPoolFiles(t, tmpPublic),
		"pool file should land under the public dir")

	stateEntries, err := os.ReadDir(tmpState)
	require.NoError(t, err)
	for _, e := range stateEntries {
		assert.NotEqual(t, "debian", e.Name(),
			"state dir must not gain a per-distro tree")
	}
}

// TestAddRejectsUnconfiguredDistro asserts that a --distro value not present
// in config.distros is rejected by requireConfiguredDistro before any pool or
// DB mutation. The fixture distro list is debian/ubuntu (from initRepoForTest);
// "raspbian" is deliberately absent.
func TestAddRejectsUnconfiguredDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "raspbian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), `"raspbian"`,
		"error should name the offending --distro value")
	assert.Contains(t, stderr.String(), "config.distros",
		"error should mention the config field the operator must edit")

	assert.Equal(t, 0, rowCount(t, tmpState), "no DB row must be inserted")
	assert.Empty(t, listPoolFiles(t, tmpPublic), "no pool file must be written")
}

// TestAddAcceptsNamespacedDistro covers the Task 3 repointing of the --distro
// validator. With distros: [customer/acme] declared in config, an
// `add --distro customer/acme` invocation must pass requireDistroScope (no
// slash rejection) and requireConfiguredDistro (lookup matches), and produce a
// pool file nested under <publicDir>/customer/acme/pool/...
func TestAddAcceptsNamespacedDistro(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	appendTestDistros(t, tmpState, "customer/acme")
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "customer/acme", "--suite", "stable",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	want := filepath.Join(tmpPublic, filepath.FromSlash(
		"customer/acme/pool/stable/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
	assert.FileExists(t, want, "pool file must land under the namespaced subtree")
	assert.Equal(t, 1, rowCount(t, tmpState))
}

// TestAddRejectsSkipExistingWithReplace guards the mutual-exclusion check at
// flag-validation time: --skip-existing and --replace encode opposite intents
// (leave collisions alone vs. overwrite them) and must never be combined.
func TestAddRejectsSkipExistingWithReplace(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm",
		"--skip-existing", "--replace",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "--skip-existing and --replace are mutually exclusive")
	assert.Equal(t, 0, rowCount(t, tmpState))
	assert.Empty(t, listPoolFiles(t, tmpPublic))
}

// TestAddSkipExistingIdenticalReAdd covers the happy path: re-adding the same
// .deb under --skip-existing must succeed, emit the plain skip line on stderr,
// keep the row count unchanged, and report `staged 0 packages, skipped 1`.
func TestAddSkipExistingIdenticalReAdd(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)
	require.Equal(t, 1, rowCount(t, tmpState))

	poolPath := filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
	require.FileExists(t, poolPath)
	preSHA := fileSHA256(t, poolPath)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stderr.String(),
		"skipping hello-gz_1.0_amd64: already present in debian/bookworm/main")
	assert.Contains(t, stdout.String(), "staged 0 packages, skipped 1")
	assert.Equal(t, 1, rowCount(t, tmpState), "DB row count must be unchanged on skip")
	assert.FileExists(t, poolPath, "skip must not delete the pre-existing pool file")
	assert.Equal(t, preSHA, fileSHA256(t, poolPath),
		"skip must not rewrite the pre-existing pool file")
}

// TestAddSkipExistingNoCollisions covers the no-op path: --skip-existing on a
// fresh NVRA behaves like a plain add — no `skipping` line, no `skipped` in
// the summary, row inserted.
func TestAddSkipExistingNoCollisions(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.NotContains(t, stderr.String(), "skipping")
	assert.Regexp(t, `\bstaged 1 packages\b`, stdout.String())
	assert.NotContains(t, stdout.String(), "skipped")
	assert.Equal(t, 1, rowCount(t, tmpState))
}

// TestAddSkipExistingAllSkipped pre-populates two NVRAs and re-adds them both
// under --skip-existing in a single batch. No new pool files must appear on
// disk between before/after snapshots.
func TestAddSkipExistingAllSkipped(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb1 := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	deb2 := fixtureDeb(t, "hello-xz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb1)
	addOnce(t, tmpState, tmpPublic, deb2)
	require.Equal(t, 2, rowCount(t, tmpState))

	before := listPoolFiles(t, tmpPublic)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		deb1, deb2,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "staged 0 packages, skipped 2")
	assert.Equal(t, 2, rowCount(t, tmpState))

	after := listPoolFiles(t, tmpPublic)
	assert.ElementsMatch(t, before, after,
		"no new pool files should appear when every package is skipped")
}

// TestAddSkipExistingUnderQuiet asserts that --quiet suppresses the trailing
// summary but the per-skip stderr line still fires.
func TestAddSkipExistingUnderQuiet(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, deb)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "--quiet", "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String(), "--quiet must suppress the summary")
	assert.Contains(t, stderr.String(),
		"skipping hello-gz_1.0_amd64: already present in debian/bookworm/main")
}

// TestAddSkipExistingMixedBatch covers the per-deb branch in the run loop:
// a batch where one NVRA is already present and the rest are fresh must skip
// the duplicate with a single stderr notice and still insert the other two,
// writing exactly the new pool files on disk.
func TestAddSkipExistingMixedBatch(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	existing := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	newA := fixtureDeb(t, "hello-xz_1.0_amd64.deb")
	newB := fixtureDeb(t, "hello-zst_1.0_amd64.deb")
	addOnce(t, tmpState, tmpPublic, existing)
	require.Equal(t, 1, rowCount(t, tmpState))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		existing, newA, newB,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	assert.Equal(t, 1, strings.Count(stderr.String(), "skipping "),
		"mixed batch must produce exactly one skip notice on stderr")
	assert.Contains(t, stderr.String(),
		"skipping hello-gz_1.0_amd64: already present in debian/bookworm/main")
	assert.Contains(t, stdout.String(), "staged 2 packages, skipped 1")
	assert.Equal(t, 3, rowCount(t, tmpState), "two new rows inserted on top of the existing one")

	assert.FileExists(t, filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/h/hello/hello-xz_1.0_amd64.deb")))
	assert.FileExists(t, filepath.Join(tmpPublic, filepath.FromSlash(
		"debian/pool/bookworm/main/f/foo/hello-zst_1.0_amd64.deb")))
}

// TestAddSkipExistingBatchRollbackOnNonSkipError covers the error-path
// distinction between errSkipped and any other processOne error inside the
// per-deb loop. A mid-batch failure (here: a bogus .deb after one skip + one
// successful insert) must still roll the whole transaction back and remove
// pool files written earlier in the same batch — only errSkipped advances the
// loop. If a refactor ever swallowed non-skip errors under --skip-existing,
// the row inserted before the failure would survive and this test would
// catch it.
func TestAddSkipExistingBatchRollbackOnNonSkipError(t *testing.T) {
	tmpState := t.TempDir()
	tmpPublic := t.TempDir()
	initRepoForTest(t, tmpState)
	existing := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	good := fixtureDeb(t, "hello-xz_1.0_amd64.deb")
	missing := filepath.Join(t.TempDir(), "definitely-missing.deb")
	addOnce(t, tmpState, tmpPublic, existing)
	require.Equal(t, 1, rowCount(t, tmpState))
	before := listPoolFiles(t, tmpPublic)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", tmpState, "--public", tmpPublic, "add",
		"--distro", "debian", "--suite", "bookworm", "--skip-existing",
		existing, good, missing,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Equal(t, 1, rowCount(t, tmpState),
		"non-skip error must roll back the in-batch insert; only the pre-existing row remains")
	assert.ElementsMatch(t, before, listPoolFiles(t, tmpPublic),
		"non-skip error must roll back pool files written earlier in the batch")
}

// TestAddSkipExistingMismatchedSHA covers the SHA256-mismatch branch of
// reportSkip: when a row already exists at the target NVRA but its stored
// SHA256 differs from the incoming .deb, --skip-existing must emit the loud
// `warning: skipping ... NVRA matches but sha256 differs` line (with both
// hash fragments) on stderr, leave the existing row's SHA256 untouched, and
// write no pool file. The synthetic-row trick avoids needing a fixture whose
// NVRA matches but bytes don't — reportSkip only reads the DB row's SHA256
// and never touches the filesystem, so a placeholder Filename is fine. The
// --quiet subtest asserts the warning still fires under --quiet because it is
// a diagnostic, not a summary line.
func TestAddSkipExistingMismatchedSHA(t *testing.T) {
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	realSHA := fileSHA256(t, deb)

	const fakeSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	const syntheticFilename = "pool/bookworm/main/h/hello-gz/synthetic_hello-gz_1.0_amd64.deb"

	cases := []struct {
		name  string
		quiet bool
	}{
		{name: "default"},
		{name: "quiet", quiet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpState := t.TempDir()
			tmpPublic := t.TempDir()
			initRepoForTest(t, tmpState)
			insertPackage(t, tmpState, repo.Package{
				Distro:       "debian",
				Suite:        "bookworm",
				Component:    "main",
				Architecture: "amd64",
				Name:         "hello-gz",
				Version:      "1.0",
				Filename:     syntheticFilename,
				Size:         11,
				MD5:          "00000000000000000000000000000000",
				SHA1:         "0000000000000000000000000000000000000000",
				SHA256:       fakeSHA,
				SHA512:       strings.Repeat("0", 128),
				Control:      "Package: hello-gz\nVersion: 1.0\nArchitecture: amd64\n",
			})
			require.Equal(t, 1, rowCount(t, tmpState))

			realPool := filepath.Join(tmpPublic, filepath.FromSlash(
				"debian/pool/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb"))
			syntheticPool := filepath.Join(tmpPublic, filepath.FromSlash(
				"debian/"+syntheticFilename))

			args := []string{"--repo", tmpState, "--public", tmpPublic}
			if tc.quiet {
				args = append(args, "--quiet")
			}
			args = append(args, "add",
				"--distro", "debian", "--suite", "bookworm", "--skip-existing", deb)

			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr, "test-rev")
			require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

			assert.Contains(t, stderr.String(), "warning: skipping hello-gz_1.0_amd64")
			assert.Contains(t, stderr.String(), "NVRA matches but sha256 differs")
			assert.Contains(t, stderr.String(), "existing="+fakeSHA)
			assert.Contains(t, stderr.String(), "new="+realSHA)
			assert.NotContains(t, stdout.String(), "warning",
				"mismatch warning must land on stderr, not stdout")
			if tc.quiet {
				assert.Empty(t, stdout.String(), "--quiet must suppress the summary")
			} else {
				assert.Contains(t, stdout.String(), "staged 0 packages, skipped 1")
			}

			r, err := repo.Open(context.Background(), filepath.Join(tmpState, dbFilename))
			require.NoError(t, err)
			t.Cleanup(func() { _ = r.Close() })
			rows, err := r.List(context.Background(), repo.Filter{})
			require.NoError(t, err)
			require.Len(t, rows, 1, "skip must not insert a new row")
			assert.Equal(t, fakeSHA, rows[0].SHA256,
				"skip must not overwrite the existing row's SHA256")
			assert.Equal(t, syntheticFilename, rows[0].Filename,
				"skip must not overwrite the existing row's Filename")

			assert.NoFileExists(t, realPool, "no pool file must be written at the real Filename")
			assert.NoFileExists(t, syntheticPool, "no pool file must be written at the synthetic Filename")
		})
	}
}

// addOnce performs a baseline successful add of a single .deb so subsequent
// scenarios can exercise duplicate / replace logic against a populated repo.
func addOnce(t *testing.T, state, public, deb string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--repo", state, "--public", public, "add",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "baseline add failed: %s", stderr.String())
}
