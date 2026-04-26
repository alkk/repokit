package pool

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPath(t *testing.T) {
	tests := []struct {
		name    string
		distro  string
		suite   string
		comp    string
		source  string
		pkgName string
		version string
		arch    string
		want    string
	}{
		{
			name:    "regular package uses first letter of source",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "hello", pkgName: "hello", version: "1.0", arch: "amd64",
			want: "pool/debian/bookworm/main/h/hello/hello_1.0_amd64.deb",
		},
		{
			name:    "lib package uses libX shard",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "libfoo", pkgName: "libfoo", version: "1.0-1", arch: "amd64",
			want: "pool/debian/bookworm/main/libf/libfoo/libfoo_1.0-1_amd64.deb",
		},
		{
			name:    "libssl3 sharded under libs",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "libssl3", pkgName: "libssl3", version: "3.0", arch: "arm64",
			want: "pool/debian/bookworm/main/libs/libssl3/libssl3_3.0_arm64.deb",
		},
		{
			name:    "empty source falls back to package name",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "", pkgName: "hello", version: "1.0", arch: "amd64",
			want: "pool/debian/bookworm/main/h/hello/hello_1.0_amd64.deb",
		},
		{
			name:    "binary name distinct from source uses source for shard and dir",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "hello", pkgName: "hello-doc", version: "1.0", arch: "all",
			want: "pool/debian/bookworm/main/h/hello/hello-doc_1.0_all.deb",
		},
		{
			name:    "mixed-case source lowercases shard but preserves dir",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "Hello", pkgName: "Hello", version: "1.0", arch: "amd64",
			want: "pool/debian/bookworm/main/h/Hello/Hello_1.0_amd64.deb",
		},
		{
			name:    "exactly-lib falls back to first letter",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "lib", pkgName: "lib", version: "1.0", arch: "amd64",
			want: "pool/debian/bookworm/main/l/lib/lib_1.0_amd64.deb",
		},
		{
			name:    "non-default component and suite",
			distro:  "ubuntu", suite: "noble", comp: "contrib",
			source: "zsh", pkgName: "zsh", version: "5.9", arch: "arm64",
			want: "pool/ubuntu/noble/contrib/z/zsh/zsh_5.9_arm64.deb",
		},
		{
			name:    "empty source and name shards to _ instead of panicking",
			distro:  "debian", suite: "bookworm", comp: "main",
			source: "", pkgName: "", version: "1.0", arch: "amd64",
			want: "pool/debian/bookworm/main/_/_1.0_amd64.deb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Path(tt.distro, tt.suite, tt.comp, tt.source, tt.pkgName, tt.version, tt.arch)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "\\", "path must use forward slashes")
		})
	}
}

func TestWriteCreatesFile(t *testing.T) {
	root := t.TempDir()
	rel := "pool/debian/bookworm/main/h/hello/hello_1.0_amd64.deb"

	require.NoError(t, Write(root, rel, strings.NewReader("payload")))

	abs := absPath(root, rel)
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(got))

	assert.NoFileExists(t, abs+".tmp", ".tmp must be cleaned up on success")
}

func TestWriteFailsOnExisting(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))

	err := Write(root, rel, strings.NewReader("v2"))
	require.Error(t, err)

	got, readErr := os.ReadFile(absPath(root, rel))
	require.NoError(t, readErr)
	assert.Equal(t, "v1", string(got), "existing file must be untouched")

	assert.NoFileExists(t, absPath(root, rel)+".tmp", ".tmp must not be left behind")
}

func TestReplaceOverwrites(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))
	backup, err := Replace(root, rel, strings.NewReader("v2-longer"))
	require.NoError(t, err)
	require.NoError(t, FinalizeReplace(backup))

	got, err := os.ReadFile(absPath(root, rel))
	require.NoError(t, err)
	assert.Equal(t, "v2-longer", string(got))
}

func TestReplaceCreatesIfAbsent(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	backup, err := Replace(root, rel, strings.NewReader("first"))
	require.NoError(t, err)
	assert.Empty(t, backup, "no prior file means no backup to track")

	got, err := os.ReadFile(absPath(root, rel))
	require.NoError(t, err)
	assert.Equal(t, "first", string(got))
}

// TestReplaceLeavesBackupForCaller pins the contract that Replace does not
// remove the .bak before returning. Cleaning it up early would close the
// recovery window between Replace returning and the surrounding transaction
// durably committing.
func TestReplaceLeavesBackupForCaller(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))

	backup, err := Replace(root, rel, strings.NewReader("v2"))
	require.NoError(t, err)
	require.NotEmpty(t, backup, "Replace must report the backup path it stashed")
	assert.FileExists(t, backup, "Replace must leave the .bak in place for the caller to finalize")
	got, err := os.ReadFile(backup)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got), ".bak must hold the prior bytes")
}

func TestFinalizeReplaceRemovesBackup(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))

	backup, err := Replace(root, rel, strings.NewReader("v2"))
	require.NoError(t, err)
	require.NoError(t, FinalizeReplace(backup))

	assert.NoFileExists(t, backup, "FinalizeReplace must remove the .bak")
	got, err := os.ReadFile(absPath(root, rel))
	require.NoError(t, err)
	assert.Equal(t, "v2", string(got), "dest must still hold the new bytes after Finalize")
}

// TestFinalizeReplaceEmptyBackupIsNoop documents that the caller can
// unconditionally call FinalizeReplace on the value Replace returned, even
// when no prior file existed (backup == "").
func TestFinalizeReplaceEmptyBackupIsNoop(t *testing.T) {
	require.NoError(t, FinalizeReplace(""))
}

// TestFinalizeReplaceMissingBackupIsNoop covers the case where the .bak
// has already been removed (e.g. by an operator clearing a poisoned
// recovery state). FinalizeReplace must not surface ErrNotExist as an
// error — the desired post-condition is "no .bak at this path", which
// already holds.
func TestFinalizeReplaceMissingBackupIsNoop(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	dest := absPath(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	require.NoError(t, FinalizeReplace(dest+".bak"))
}

// TestFinalizeReplaceSurfacesRemovalFailure pins the contract that a
// failed os.Remove is reported back to the caller rather than silently
// swallowed. Without this, a stale .bak would persist and poison the next
// Replace at the same dest (see pool.stashExisting). The simulation uses
// a directory at the .bak path so os.Remove fails with a non-ENOENT
// error on every supported platform.
func TestFinalizeReplaceSurfacesRemovalFailure(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	dest := absPath(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	backup := dest + ".bak"
	require.NoError(t, os.Mkdir(backup, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(backup, "blocker"), []byte("x"), 0o644))

	err := FinalizeReplace(backup)
	require.Error(t, err, "FinalizeReplace must surface a non-ENOENT removal failure")
	assert.Contains(t, err.Error(), "removing backup")
}

// TestRollbackReplaceRestoresPriorBytes covers the commit-failure path: the
// new dest must be torn down and the .bak put back so the on-disk pool
// matches the rolled-back DB row.
func TestRollbackReplaceRestoresPriorBytes(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))

	backup, err := Replace(root, rel, strings.NewReader("v2"))
	require.NoError(t, err)
	require.NotEmpty(t, backup)

	RollbackReplace(root, rel, backup)

	got, err := os.ReadFile(absPath(root, rel))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got), "RollbackReplace must restore the prior bytes")
	assert.NoFileExists(t, backup, "the .bak must move back to dest, not stay behind")
}

// TestRollbackReplaceWithoutBackupDeletesDest covers the no-prior-file path
// (Write mode or a Replace where dest did not exist): backup is "" and the
// rollback degrades to a plain delete.
func TestRollbackReplaceWithoutBackupDeletesDest(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("v1")))

	RollbackReplace(root, rel, "")

	assert.NoFileExists(t, absPath(root, rel), "RollbackReplace with no backup must remove dest")
}

// TestReplacePreservesExistingBackupAsRecovery covers the
// crash-between-Replace-and-tx-commit retry path: the on-disk dest holds
// uncommitted bytes from the prior crashed run while the .bak still holds
// the original bytes that match the rolled-back DB row. The next Replace
// must keep the .bak intact (not clobber it with the uncommitted dest) so
// a subsequent rollback can restore the truly original bytes.
func TestReplacePreservesExistingBackupAsRecovery(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	dest := absPath(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	require.NoError(t, os.WriteFile(dest, []byte("uncommitted-v2"), 0o644))
	require.NoError(t, os.WriteFile(dest+".bak", []byte("original-v1"), 0o644))

	backup, err := Replace(root, rel, strings.NewReader("v3"))
	require.NoError(t, err)
	require.NotEmpty(t, backup)

	bakBytes, err := os.ReadFile(backup)
	require.NoError(t, err)
	assert.Equal(t, "original-v1", string(bakBytes),
		"existing .bak must be preserved verbatim, not overwritten with uncommitted dest")

	RollbackReplace(root, rel, backup)
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "original-v1", string(got),
		"rollback must restore the truly original bytes, not the uncommitted ones")
}

// TestReconcileBackupRemovesStaleWhenCommitted covers the crash-after-commit
// scenario: dest holds the committed bytes and a stale .bak from before the
// crash is left behind. Reconciliation with committed=true must remove the
// .bak so the next Replace's stashExisting cannot mistake it for recovery
// material.
func TestReconcileBackupRemovesStaleWhenCommitted(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	dest := absPath(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	require.NoError(t, os.WriteFile(dest, []byte("committed-v2"), 0o644))
	require.NoError(t, os.WriteFile(dest+".bak", []byte("stale-v1"), 0o644))

	require.NoError(t, ReconcileBackup(root, rel, true))

	assert.NoFileExists(t, dest+".bak", "stale .bak must be removed when committed=true")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "committed-v2", string(got), "dest must be left untouched")
}

// TestReconcileBackupRestoresWhenUncommitted covers the inverse: a prior Tx
// aborted before .bak was reconciled, so dest may hold uncommitted bytes and
// .bak holds the bytes that match the rolled-back DB row.
func TestReconcileBackupRestoresWhenUncommitted(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	dest := absPath(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	require.NoError(t, os.WriteFile(dest, []byte("uncommitted-v2"), 0o644))
	require.NoError(t, os.WriteFile(dest+".bak", []byte("original-v1"), 0o644))

	require.NoError(t, ReconcileBackup(root, rel, false))

	assert.NoFileExists(t, dest+".bak", ".bak must be moved over dest")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "original-v1", string(got), "dest must be restored from .bak")
}

// TestReconcileBackupNoopWithoutBackup pins that the function is safe to call
// unconditionally — a missing .bak short-circuits regardless of the committed
// flag.
func TestReconcileBackupNoopWithoutBackup(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, ReconcileBackup(root, rel, true))
	require.NoError(t, ReconcileBackup(root, rel, false))
}

func TestDeleteRemovesFile(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"
	require.NoError(t, Write(root, rel, strings.NewReader("payload")))

	require.NoError(t, Delete(root, rel))

	assert.NoFileExists(t, absPath(root, rel))
}

func TestDeleteIdempotent(t *testing.T) {
	root := t.TempDir()
	rel := "pool/missing/never-existed.deb"

	require.NoError(t, Delete(root, rel))
	require.NoError(t, Delete(root, rel))
}

func TestDeleteRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("keep me"), 0o644))

	err := Delete(root, "../"+filepath.Base(outside))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes repo root")
	assert.FileExists(t, outside, "traversal target must not be removed")
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../etc/passwd", "a/../../escape", "../../../etc/passwd"} {
		_, err := SafeJoin(root, rel)
		require.Error(t, err, "expected rejection for %q", rel)
	}
}

func TestWriteCleansTmpOnReaderError(t *testing.T) {
	root := t.TempDir()
	rel := "pool/d/s/c/x/x/x_1_amd64.deb"

	r := &erroringReader{data: []byte("partial-bytes-then-boom")}
	err := Write(root, rel, r)
	require.Error(t, err)

	dest := absPath(root, rel)
	assert.NoFileExists(t, dest, "no destination must be left after partial write")
	assert.NoFileExists(t, dest+".tmp", "no .tmp must be left after partial write")
}

func TestWriteCreatesParentDirectories(t *testing.T) {
	root := t.TempDir()
	rel := "pool/deep/nested/path/that/does/not/exist/x_1_amd64.deb"

	require.NoError(t, Write(root, rel, strings.NewReader("ok")))

	info, err := os.Stat(filepath.Dir(absPath(root, rel)))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func absPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// erroringReader returns its data on the first Read and io.ErrUnexpectedEOF
// thereafter. Used to simulate a torn copy mid-write.
type erroringReader struct {
	data []byte
	done bool
}

func (r *erroringReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.done = true
	if n < len(r.data) {
		return n, nil
	}
	// returning n with no error here would let io.Copy issue another Read,
	// which then returns the error — same effect, slightly less direct.
	return n, io.ErrUnexpectedEOF
}
