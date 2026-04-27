package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netxms/debrepo/pkg/pool"
	"github.com/netxms/debrepo/pkg/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

// insertPackage writes one row directly into the repo, bypassing add. Used by
// publish tests that need shapes (e.g. arch=arm64 or arch=all) for which we
// don't have a real .deb fixture.
func insertPackage(t *testing.T, root string, p repo.Package) {
	t.Helper()
	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Tx(context.Background(), func(tx *sql.Tx) error {
		return r.Insert(context.Background(), tx, p)
	}))
}

// fakePackage builds a Package whose hashes/size are valid for the supplied
// body, with a minimal control stanza. The pool file is also written so a
// caller can verify atomic swaps don't disturb pool/. Component is fixed at
// "main" — multi-component scenarios live in the dedicated integration test.
func fakePackage(t *testing.T, root, distro, suite, arch, name, version string) repo.Package {
	t.Helper()
	const component = "main"
	body := []byte("fake .deb body for " + name + "_" + version + "_" + arch)
	filename := pool.Filename(suite, component, name, name, version, arch)
	full := filepath.Join(root, filepath.FromSlash(pool.DiskPath(distro, filename)))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, body, 0o644))

	control := "Package: " + name + "\n" +
		"Version: " + version + "\n" +
		"Architecture: " + arch + "\n" +
		"Maintainer: Test <test@example.com>\n" +
		"Description: fake " + name + "\n"

	return repo.Package{
		Distro:       distro,
		Suite:        suite,
		Component:    component,
		Architecture: arch,
		Name:         name,
		Version:      version,
		Filename:     filename,
		Size:         int64(len(body)),
		MD5:          hashHex(md5.New(), body),
		SHA1:         hashHex(sha1.New(), body),
		SHA256:       hashHex(sha256.New(), body),
		SHA512:       hashHex(sha512.New(), body),
		Control:      control,
	}
}

func hashHex(h hash.Hash, body []byte) string {
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func TestPublishEmptySuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	releasePath := filepath.Join(root, "debian", "dists", "bookworm", "Release")
	body, err := os.ReadFile(releasePath)
	require.NoError(t, err)
	out := string(body)

	assert.Contains(t, out, "Architectures: \n")
	assert.Contains(t, out, "Components: \n")
	for _, block := range []string{"MD5Sum:\n", "SHA1:\n", "SHA256:\n", "SHA512:\n"} {
		assert.Contains(t, out, block)
	}
	for line := range strings.SplitSeq(out, "\n") {
		assert.Falsef(t, strings.HasPrefix(line, " "),
			"empty-suite Release should have no checksum entries: %q", line)
	}

	assert.FileExists(t, filepath.Join(root, "debian", "dists", "bookworm", "InRelease"))
	assert.FileExists(t, filepath.Join(root, "debian", "dists", "bookworm", "Release.gpg"))
	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm.new"))
	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm.old"))
}

func TestPublishMultiArch(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "arm64", "alpha", "1.0"))
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "all", "common", "2.0"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	dists := filepath.Join(root, "debian", "dists", "bookworm")

	assert.NoDirExists(t, filepath.Join(dists, "..", "bookworm.new"))
	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "binary-all"))

	for _, arch := range []string{"amd64", "arm64"} {
		dir := filepath.Join(dists, "main", "binary-"+arch)
		body, err := os.ReadFile(filepath.Join(dir, "Packages"))
		require.NoErrorf(t, err, "missing Packages for %s", arch)
		txt := string(body)
		assert.Containsf(t, txt, "Package: alpha\n", "%s should include arch row", arch)
		assert.Containsf(t, txt, "Package: common\n", "%s should include arch=all row", arch)
	}

	releaseBody, err := os.ReadFile(filepath.Join(dists, "Release"))
	require.NoError(t, err)
	out := string(releaseBody)
	assert.Contains(t, out, "Architectures: amd64 arm64\n")
	assert.NotContains(t, out, "Architectures: all")
	assert.Contains(t, out, "Components: main\n")

	for _, arch := range []string{"amd64", "arm64"} {
		for _, base := range []string{"Packages", "Packages.gz", "Packages.xz"} {
			assert.Containsf(t, out, "main/binary-"+arch+"/"+base,
				"Release should reference %s/%s", arch, base)
		}
	}
}

// TestPublishRejectsArchAllOnlySuite covers the edge case where every package
// in a suite has arch=all. Without at least one concrete-arch package, no
// binary-<arch>/Packages file is produced and the arch=all rows would be
// silently invisible to apt clients. publish must reject this scope with a
// clear error rather than emit an empty Release.
func TestPublishRejectsArchAllOnlySuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "all", "common", "1.0"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "arch=all")
	assert.Contains(t, stderr.String(), "concrete-architecture")

	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm"))
}

func TestPublishMissingDistroOrSuite(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "distro only", args: []string{"--distro", "debian"}},
		{name: "suite only", args: []string{"--suite", "bookworm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initRepoForTest(t, root)

			full := append([]string{"--repo", root, "publish"}, tc.args...)
			full = append(full, "--no-sign")
			var stdout, stderr bytes.Buffer
			code := Run(full, &stdout, &stderr, "test-rev")
			assert.Equal(t, ExitError, code)
			assert.Contains(t, stderr.String(), "must be provided together")
		})
	}
}

func TestPublishAllScopesWhenBothEmpty(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))
	insertPackage(t, root, fakePackage(t, root, "ubuntu", "noble", "amd64", "bravo", "2.0"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "publish", "--no-sign"},
		&stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "published 2 suite(s)")

	assert.FileExists(t, filepath.Join(root, "debian", "dists", "bookworm", "Release"))
	assert.FileExists(t, filepath.Join(root, "ubuntu", "dists", "noble", "Release"))
}

// TestPublishAllScopesIncludesEmptiedSuites guards against `debrepo publish`
// (no scope) skipping a suite that was emptied by `delete`. The dists tree
// must be republished so the stale Packages/Release files do not keep
// pointing at packages that no longer exist.
func TestPublishAllScopesIncludesEmptiedSuites(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

	args := []string{"--repo", root, "publish", "--no-sign"}
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "first publish: %s", stderr.String())

	pkgPath := filepath.Join(root, "debian", "dists", "bookworm", "main", "binary-amd64", "Packages")
	body, err := os.ReadFile(pkgPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Package: alpha\n")

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	require.NoError(t, r.Tx(context.Background(), func(tx *sql.Tx) error {
		_, derr := r.Delete(context.Background(), tx, repo.Filter{
			Distro: "debian", Suite: "bookworm",
		})
		if derr != nil {
			return fmt.Errorf("deleting rows: %w", derr)
		}
		return nil
	}))
	require.NoError(t, r.Close())

	stdout.Reset()
	stderr.Reset()
	code = Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "second publish: %s", stderr.String())
	assert.Contains(t, stdout.String(), "published 1 suite(s)")

	assert.NoFileExists(t, pkgPath,
		"binary-arch tree should be wiped when the suite has no packages")

	releaseBody, err := os.ReadFile(filepath.Join(root, "debian", "dists", "bookworm", "Release"))
	require.NoError(t, err)
	out := string(releaseBody)
	assert.Contains(t, out, "Architectures: \n")
	assert.Contains(t, out, "Components: \n")
}

// TestPublishAllScopesIgnoresStrayDirsWithoutRelease guards against the
// no-flags publish wiping a hand-created directory under dists/. Only suites
// previously published (i.e. carrying a Release file) are valid republish
// targets discovered from the filesystem.
func TestPublishAllScopesIgnoresStrayDirsWithoutRelease(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	stray := filepath.Join(root, "debian", "dists", "notes")
	require.NoError(t, os.MkdirAll(stray, 0o755))
	strayFile := filepath.Join(stray, "readme.txt")
	require.NoError(t, os.WriteFile(strayFile, []byte("not managed by debrepo\n"), 0o600))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "publish", "--no-sign"},
		&stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "no suites to publish")

	assert.DirExists(t, stray, "stray directory must remain after no-op publish")
	assert.FileExists(t, strayFile, "stray file inside must remain after no-op publish")
}

func TestPublishAtomicSwapReplacesLive(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

	args := []string{"--repo", root, "publish", "--distro", "debian", "--suite", "bookworm", "--no-sign"}
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "first publish: %s", stderr.String())

	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "2.0"))

	stdout.Reset()
	stderr.Reset()
	code = Run(args, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "second publish: %s", stderr.String())

	body, err := os.ReadFile(filepath.Join(root, "debian", "dists", "bookworm",
		"main", "binary-amd64", "Packages"))
	require.NoError(t, err)
	txt := string(body)
	assert.Contains(t, txt, "Version: 1.0\n")
	assert.Contains(t, txt, "Version: 2.0\n")

	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm.new"))
	assert.NoDirExists(t, filepath.Join(root, "debian", "dists", "bookworm.old"))
}

// TestPublishCleansStaleStagingDirs ensures publish wipes any leftover
// <suite>.new and <suite>.old siblings from a prior crashed run before
// staging the new build.
func TestPublishCleansStaleStagingDirs(t *testing.T) {
	cases := []struct {
		name         string
		makeStaleNew bool
		makeStaleOld bool
	}{
		{name: "stale new only", makeStaleNew: true},
		{name: "stale old only", makeStaleOld: true},
		{name: "both stale", makeStaleNew: true, makeStaleOld: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initRepoForTest(t, root)
			insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

			distsParent := filepath.Join(root, "debian", "dists")
			require.NoError(t, os.MkdirAll(distsParent, 0o755))
			newPath := filepath.Join(distsParent, "bookworm.new")
			oldPath := filepath.Join(distsParent, "bookworm.old")
			if tc.makeStaleNew {
				require.NoError(t, os.MkdirAll(newPath, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(newPath, "stale.txt"), []byte("crash-leftover"), 0o644))
			}
			if tc.makeStaleOld {
				require.NoError(t, os.MkdirAll(oldPath, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(oldPath, "stale.txt"), []byte("crash-leftover"), 0o644))
			}

			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"--repo", root, "publish",
				"--distro", "debian", "--suite", "bookworm", "--no-sign",
			}, &stdout, &stderr, "test-rev")
			require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

			assert.NoDirExists(t, newPath, "stale .new must be removed")
			assert.NoDirExists(t, oldPath, "stale .old must be removed")

			assert.FileExists(t, filepath.Join(distsParent, "bookworm", "Release"))
		})
	}
}

// TestPublishGzAndXzMatchPlain decompresses Packages.gz and Packages.xz and
// asserts they round-trip to byte-identical Packages bytes — guards against
// fan-out writer bugs in WritePackagesAll seen through the publish command.
func TestPublishGzAndXzMatchPlain(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	dir := filepath.Join(root, "debian", "dists", "bookworm", "main", "binary-amd64")
	plain, err := os.ReadFile(filepath.Join(dir, "Packages"))
	require.NoError(t, err)

	gzBytes, err := os.ReadFile(filepath.Join(dir, "Packages.gz"))
	require.NoError(t, err)
	gzr, err := gzip.NewReader(bytes.NewReader(gzBytes))
	require.NoError(t, err)
	defer gzr.Close()
	gzDecoded, err := io.ReadAll(gzr)
	require.NoError(t, err)
	assert.Equal(t, plain, gzDecoded)

	xzBytes, err := os.ReadFile(filepath.Join(dir, "Packages.xz"))
	require.NoError(t, err)
	xzr, err := xz.NewReader(bytes.NewReader(xzBytes))
	require.NoError(t, err)
	xzDecoded, err := io.ReadAll(xzr)
	require.NoError(t, err)
	assert.Equal(t, plain, xzDecoded)
}

// TestPublishAllScopesSkipsInvalidDiscoveredScopeNames ensures that
// filesystem-discovered suite directories with names rejected by
// validateScopeName (e.g. starting with '-' or '.') are skipped instead of
// silently published as unvalidated scopes. Such names cannot be created via
// `add` and should not be picked up by a no-flags publish either.
func TestPublishAllScopesSkipsInvalidDiscoveredScopeNames(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	stray := filepath.Join(root, "debian", "dists", "-evil")
	require.NoError(t, os.MkdirAll(stray, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stray, "Release"), []byte("Origin: forged\n"), 0o644))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "publish", "--no-sign"},
		&stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "no suites to publish")

	assert.DirExists(t, stray, "scope skipped by validation must remain untouched")
	assert.NoDirExists(t, stray+suiteSuffixNew)
	assert.NoDirExists(t, stray+suiteSuffixOld)
}

// TestPublishAllScopesSkipsInvalidDBScopeNames mirrors the filesystem-scope
// guard for the database side: rows whose distro/suite would not pass
// validateScopeName (control chars, '..' segment, reserved staging suffix,
// etc.) must not be materialized into filesystem ops. `add` validates on
// the way in, so this only triggers if a row was written by external SQL
// or a bug-bypass path; the publish loop must still refuse to derive
// dists/<distro>/<suite> from such names.
func TestPublishAllScopesSkipsInvalidDBScopeNames(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	good := fakePackage(t, root, "debian", "bookworm", "amd64", "hello", "1.0")
	insertPackage(t, root, good)

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	_, err = r.DB().ExecContext(context.Background(),
		`INSERT INTO packages (distro, suite, component, architecture, name, version,
			filename, size, md5, sha1, sha256, sha512, control, added_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"debian", "..", "main", "amd64", "ghost", "1.0",
		"pool/debian/x/main/g/ghost/ghost_1.0_amd64.deb", int64(0),
		"d41d8cd98f00b204e9800998ecf8427e",
		"da39a3ee5e6b4b0d3255bfef95601890afd80709",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e",
		"Package: ghost\n", time.Now().Unix())
	require.NoError(t, err)
	require.NoError(t, r.Close())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "publish", "--no-sign"},
		&stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "published 1 suite(s)")

	assert.DirExists(t, filepath.Join(root, "debian", "dists", "bookworm"))
	// Without the dbScopes filter, publishScope on (debian, "..") would
	// resolve livePath to root/debian, leaving sibling staging dirs like
	// root/debian.new and root/debian.old as evidence; their absence proves
	// the bad row never reached the swap logic.
	assert.NoDirExists(t, filepath.Join(root, "debian.new"))
	assert.NoDirExists(t, filepath.Join(root, "debian.old"))
}

// TestSwapDistsRestoresLiveOnPromoteFailure ensures that when the .new → live
// rename fails after live has already been demoted to .old, the previous live
// tree is moved back to its original path. Without rollback, the next publish
// run's cleanStaging would wipe .old and the previously-live tree would be
// permanently lost.
func TestSwapDistsRestoresLiveOnPromoteFailure(t *testing.T) {
	root := t.TempDir()
	distsParent := filepath.Join(root, "debian", "dists")
	live := filepath.Join(distsParent, "bookworm")
	old := live + suiteSuffixOld
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "marker"), []byte("original-live"), 0o644))

	fresh := filepath.Join(distsParent, "bookworm.does-not-exist")

	err := swapDists(distsParent, live, fresh, old)
	require.Error(t, err, "rename of nonexistent fresh path must fail")

	body, readErr := os.ReadFile(filepath.Join(live, "marker"))
	require.NoError(t, readErr, "live tree must be restored after failed promote")
	assert.Equal(t, []byte("original-live"), body)
	assert.NoDirExists(t, old, "previous live must not be left behind at .old")
}

// TestPublishRecordsLastPublishTimestamp guards against setLastPublish silently
// dropping the meta key — without an end-to-end assertion, a refactor could
// remove the call and tests would still pass.
func TestPublishRecordsLastPublishTimestamp(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

	before := time.Now().Unix()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	after := time.Now().Unix()

	r, err := repo.Open(context.Background(), filepath.Join(root, dbFilename))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	var raw string
	require.NoError(t, r.DB().QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, metaLastPublishKey).Scan(&raw))
	got, perr := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, perr, "last_publish_at must parse as int64 unix seconds")
	assert.GreaterOrEqual(t, got, before, "timestamp must not predate the publish call")
	assert.LessOrEqual(t, got, after, "timestamp must not postdate the publish call")
}

// TestPublishRejectsMalformedConfig covers the loadPublishConfig parse-error
// branch — without it, a regression that swallows the YAML error would let
// publish proceed against a partially-populated config.
func TestPublishRejectsMalformedConfig(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"),
		[]byte("origin: ok\nsuites: [oops"), 0o600))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm", "--no-sign",
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "loading config")
}

// TestPublishRequiresSigningKeyWithoutNoSign covers pickSigner's empty-key
// guard — every other publish test uses --no-sign, so the failure path is
// otherwise unexercised. The starter config.yaml ships with a non-empty
// REPLACE_ME, so the test removes it to drive loadPublishConfig down the
// "no config" branch where SigningKey defaults to empty.
func TestPublishRequiresSigningKeyWithoutNoSign(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))
	require.NoError(t, os.Remove(filepath.Join(root, "config.yaml")))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "publish",
		"--distro", "debian", "--suite", "bookworm",
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "signing_key")
	assert.Contains(t, stderr.String(), "--no-sign")
}

// TestCleanStagingRecoversLiveFromOldWhenLiveMissing simulates the
// double-failure scenario where swapDists demoted live to .old, the .new →
// live promotion failed, and the .old → live rollback ALSO failed (e.g.
// transient EROFS). The next publish run finds live missing but .old present;
// without recovery, blanket-removing .old would permanently lose the only
// remaining copy of the previous live tree.
func TestCleanStagingRecoversLiveFromOldWhenLiveMissing(t *testing.T) {
	parent := t.TempDir()
	live := filepath.Join(parent, "bookworm")
	old := live + suiteSuffixOld
	newPath := live + suiteSuffixNew

	require.NoError(t, os.MkdirAll(old, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "marker"), []byte("previous-live"), 0o644))
	require.NoError(t, os.MkdirAll(newPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(newPath, "stale"), []byte("partial"), 0o644))

	require.NoError(t, cleanStaging(live, newPath, old))

	body, err := os.ReadFile(filepath.Join(live, "marker"))
	require.NoError(t, err, "previous live tree must be promoted from .old")
	assert.Equal(t, []byte("previous-live"), body)
	assert.NoDirExists(t, old, ".old must be consumed by the recovery rename")
	assert.NoDirExists(t, newPath, "stale .new must still be removed")
}

// TestPublishSerializesWithConcurrentWriter guards two races: a concurrent
// add/delete committing between publish's per-(component, arch) DB reads
// would yield indexes assembled from inconsistent snapshots, and two
// concurrent publishes on the same suite would collide in cleanStaging /
// swapDists. Holding a writer lock from a separate connection (mimicking
// either case) must block publish until release.
func TestPublishSerializesWithConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	insertPackage(t, root, fakePackage(t, root, "debian", "bookworm", "amd64", "alpha", "1.0"))

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

	publishDone := make(chan struct {
		code   int
		stderr string
	}, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"--repo", root, "publish", "--no-sign",
			"--distro", "debian", "--suite", "bookworm",
		}, &stdout, &stderr, "test-rev")
		publishDone <- struct {
			code   int
			stderr string
		}{code, stderr.String()}
	}()

	select {
	case res := <-publishDone:
		t.Fatalf("publish completed while writer lock was held: code=%d stderr=%s", res.code, res.stderr)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	wg.Wait()

	select {
	case res := <-publishDone:
		require.Equal(t, ExitOK, res.code, "stderr: %s", res.stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not finish after writer lock release")
	}
}
