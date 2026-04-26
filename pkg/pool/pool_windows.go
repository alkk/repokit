//go:build windows

package pool

// fsyncDir is a no-op on Windows, where opening a directory for sync is not
// supported. The preceding os.Rename already guarantees ordering on NTFS.
func fsyncDir(_ string) error {
	return nil
}
