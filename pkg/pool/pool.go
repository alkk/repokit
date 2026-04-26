// Package pool manages the on-disk physical .deb files under <root>/pool/.
// Paths returned and accepted by this package use forward slashes regardless
// of OS — they are repo-relative and serialized verbatim into Packages files.
package pool

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// poolDir is the top-level directory under the repo root holding physical .debs.
const poolDir = "pool"

// Path returns the repo-relative pool path for a .deb identified by its
// coordinates. Source falls back to name when empty. Names beginning with
// "lib" are sharded under "lib<second-letter>" per Debian convention; all
// other names use the lowercased first letter as shard.
func Path(distro, suite, component, source, name, version, arch string) string {
	if source == "" {
		source = name
	}
	filename := fmt.Sprintf("%s_%s_%s.deb", name, version, arch)
	return path.Join(poolDir, distro, suite, component, shardLetter(source), source, filename)
}

// shardLetter returns the pool sub-directory shard for a source name.
// "libfoo" → "libf"; "hello" → "h". Falls back to first letter when "lib"
// is unaccompanied by further characters; an empty source yields "_" so
// callers cannot panic on a malformed input that bypassed validation.
func shardLetter(source string) string {
	lower := strings.ToLower(source)
	if lower == "" {
		return "_"
	}
	if strings.HasPrefix(lower, "lib") && len(lower) > 3 {
		return lower[:4]
	}
	return lower[:1]
}

// Write streams src to <root>/<relPath>.tmp, fsyncs the file, renames it to
// the final destination, then fsyncs the parent directory. It returns an
// error if the destination already exists.
func Write(root, relPath string, src io.Reader) error {
	_, err := writeFile(root, relPath, src, false)
	return err
}

// Replace behaves like Write but overwrites an existing destination. It
// returns the path of the backup (.bak) holding the prior bytes, or "" when
// there was no prior file. The caller MUST call FinalizeReplace on commit
// success or RollbackReplace on commit failure: leaving the .bak in place
// keeps the prior bytes recoverable across the window between Replace
// returning and the surrounding transaction durably committing.
func Replace(root, relPath string, src io.Reader) (string, error) {
	return writeFile(root, relPath, src, true)
}

// FinalizeReplace removes the backup created by Replace after the
// surrounding transaction has committed. The error is reported but is not
// itself fatal: the new pool bytes are already durable, so a cleanup
// failure does not justify rolling back the committed transaction. The
// caller should still surface the error — a stale .bak is treated as
// canonical recovery material by the next Replace at the same dest (see
// stashExisting), which would silently restore stale bytes on a later
// failure unless an operator clears the leftover first.
func FinalizeReplace(backup string) error {
	if backup == "" {
		return nil
	}
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing backup %s: %w", backup, err)
	}
	dir := filepath.Dir(backup)
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

// RollbackReplace undoes a successful Replace after the surrounding
// transaction failed: it removes the newly promoted dest and, when a backup
// exists, restores the prior bytes. Best-effort — the caller is already
// reporting the original commit error and a cleanup failure here would only
// drown that out. A backup of "" reduces this to a plain delete, so callers
// can route both Write and Replace rollbacks through the same path.
func RollbackReplace(root, relPath, backup string) {
	dest := filepath.Join(root, filepath.FromSlash(relPath))
	_ = os.Remove(dest)
	if backup != "" {
		_ = os.Rename(backup, dest)
	}
	_ = fsyncDir(filepath.Dir(dest))
}

// Delete removes the named pool file. A missing file is not an error.
// Refuses paths that resolve outside root so a corrupted DB row containing
// `..` segments cannot trick a caller (e.g. verify --gc) into deleting
// arbitrary files the process has access to.
func Delete(root, relPath string) error {
	dest, err := SafeJoin(root, relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", relPath, err)
	}
	return nil
}

// SafeJoin resolves relPath against root and returns the absolute path,
// rejecting inputs whose resolved path escapes root via `..` segments or
// absolute components. Pool-relative paths are stored in the DB and walked
// from disk; this helper is the choke point for ensuring any such path is
// kept under <root>/ before opening or deleting it.
func SafeJoin(root, relPath string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", root, err)
	}
	dest := filepath.Join(rootAbs, filepath.FromSlash(relPath))
	rel, err := filepath.Rel(rootAbs, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes repo root", relPath)
	}
	return dest, nil
}

// FsyncDir flushes the directory entry at dir so a prior rename is durable
// across power loss. On Windows it is a no-op.
func FsyncDir(dir string) error {
	return fsyncDir(dir)
}

// FsyncTreeDirs walks root and fsyncs every directory it finds. Files inside
// the tree must already be fsynced individually; this call only makes their
// directory entries (and the entries of nested directories) durable. Use it
// before an atomic rename promotes a staging tree so a crash after the
// rename cannot leave the live tree missing files that were written but
// whose dir entries had not been flushed.
func FsyncTreeDirs(root string) error {
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return fsyncDir(p)
	})
	if walkErr != nil {
		return fmt.Errorf("fsync tree %s: %w", root, walkErr)
	}
	return nil
}

// MkdirAllSync creates leafDir (and any missing ancestors), then fsyncs each
// newly-created dir's parent so the chain of new directory entries survives a
// crash. Plain os.MkdirAll only guarantees the dirs exist in the kernel cache;
// without these fsyncs a power loss after the surrounding transaction commits
// can leave a durable DB row pointing at a path whose parent dirs vanished.
// Walks upward only as far as the first existing ancestor, so the
// already-durable filesystem above leafDir is not touched.
func MkdirAllSync(leafDir string) error {
	var newDirs []string // outermost first
	cur := leafDir
	for {
		info, err := os.Stat(cur)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists but is not a directory", cur)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		newDirs = append([]string{cur}, newDirs...)
		parent := filepath.Dir(cur)
		if parent == cur {
			return fmt.Errorf("walked past filesystem root creating %s", leafDir)
		}
		cur = parent
	}
	if len(newDirs) == 0 {
		return nil
	}
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", leafDir, err)
	}
	for _, d := range newDirs {
		if err := fsyncDir(filepath.Dir(d)); err != nil {
			return fmt.Errorf("fsync %s: %w", filepath.Dir(d), err)
		}
	}
	return nil
}

// ReconcileBackup resolves a leftover <dest>.bak that survived a crash
// between Replace and the surrounding transaction settling. The caller
// supplies whether the destination is canonical per the durable DB state:
//
//   - committed=true: the prior Tx committed and FinalizeReplace never ran.
//     dest holds the canonical bytes; .bak is stale garbage and is removed.
//     Without this step, a later Replace would be tricked by stashExisting
//     into preserving .bak as recovery material, and a subsequent rollback
//     would silently restore bytes from two versions ago.
//   - committed=false: the prior Tx aborted before .bak was reconciled.
//     dest may hold uncommitted bytes from the crashed run; .bak holds the
//     bytes that match the rolled-back row, so .bak is moved over dest.
//
// A missing .bak is a no-op so callers can invoke this unconditionally.
func ReconcileBackup(root, relPath string, committed bool) error {
	dest := filepath.Join(root, filepath.FromSlash(relPath))
	backup := dest + ".bak"
	if _, err := os.Lstat(backup); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", backup, err)
	}
	dir := filepath.Dir(dest)
	if committed {
		if rerr := os.Remove(backup); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("removing stale backup %s: %w", backup, rerr)
		}
		return fsyncDir(dir)
	}
	if _, derr := os.Lstat(dest); derr == nil {
		if rerr := os.Remove(dest); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("removing uncommitted %s: %w", dest, rerr)
		}
	} else if !errors.Is(derr, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dest, derr)
	}
	if rerr := os.Rename(backup, dest); rerr != nil {
		return fmt.Errorf("restoring %s -> %s: %w", backup, dest, rerr)
	}
	return fsyncDir(dir)
}

func writeFile(root, relPath string, src io.Reader, overwrite bool) (string, error) {
	dest := filepath.Join(root, filepath.FromSlash(relPath))
	if !overwrite {
		if err := errIfExists(dest, relPath); err != nil {
			return "", err
		}
	}

	dir := filepath.Dir(dest)
	if err := MkdirAllSync(dir); err != nil {
		return "", err
	}

	tmp := dest + ".tmp"
	if err := streamToTmp(tmp, src); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	// In overwrite mode we move the existing file aside before promoting
	// tmp so any later failure (rename or fsync) can restore the prior
	// bytes — without this, a transactional caller that rolls back its DB
	// row on error would leave the on-disk pool out of sync with the row.
	backup, err := stashExisting(dest, overwrite)
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backing up %s: %w", relPath, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		restoreBackup(backup, dest)
		return "", fmt.Errorf("renaming %s: %w", relPath, err)
	}

	if err := fsyncDir(dir); err != nil {
		_ = os.Remove(dest)
		restoreBackup(backup, dest)
		return "", fmt.Errorf("fsync %s: %w", dir, err)
	}

	// The .bak is intentionally left on disk so the caller can roll back
	// the surrounding transaction — see FinalizeReplace / RollbackReplace.
	return backup, nil
}

// stashExisting renames a present dest aside to dest+".bak" so the prior
// bytes are recoverable until the new write is durable. Returns "" when
// there is nothing to back up (no existing file or non-overwrite mode).
//
// A pre-existing .bak is preserved as canonical recovery material rather
// than overwritten: it represents the prior bytes from an earlier Replace
// whose surrounding transaction did not commit, so dest may now hold the
// uncommitted bytes from that crashed run. Removing the .bak and backing
// up dest in its place would silently destroy the only copy that still
// matches the rolled-back DB row. Callers must run ReconcileBackup with
// the durable DB state before invoking Replace so a .bak left behind by a
// crashed FinalizeReplace (where dest is the committed bytes and .bak is
// stale garbage) does not slip through this branch.
func stashExisting(dest string, overwrite bool) (string, error) {
	if !overwrite {
		return "", nil
	}
	backup := dest + ".bak"
	if _, err := os.Lstat(backup); err == nil {
		if rerr := os.Remove(dest); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return "", fmt.Errorf("removing %s: %w", dest, rerr)
		}
		return backup, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", backup, err)
	}
	if _, err := os.Lstat(dest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", dest, err)
	}
	if err := os.Rename(dest, backup); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", dest, backup, err)
	}
	return backup, nil
}

// restoreBackup moves backup back to dest after a failed promotion. It is
// a best-effort cleanup — the original error is the one returned to the
// caller, so silently swallowing a failed restore would mislead them. We
// log nothing because pool callers don't have a logger; instead the next
// publish would surface the missing pool file via verify.
func restoreBackup(backup, dest string) {
	if backup == "" {
		return
	}
	_ = os.Rename(backup, dest)
	_ = fsyncDir(filepath.Dir(dest))
}

// errIfExists returns a non-nil error when dest is already present.
func errIfExists(dest, relPath string) error {
	_, err := os.Lstat(dest)
	if err == nil {
		return fmt.Errorf("pool: %s already exists", relPath)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", relPath, err)
	}
	return nil
}

// streamToTmp creates tmp, copies src into it, then fsyncs and closes the
// file handle. The caller is responsible for removing tmp on failure.
func streamToTmp(tmp string, src io.Reader) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Base(tmp), err)
	}
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", filepath.Base(tmp), err)
	}
	return nil
}
