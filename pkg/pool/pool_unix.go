//go:build !windows

package pool

import (
	"fmt"
	"os"
)

// fsyncDir flushes the directory entry so the prior rename is durable.
func fsyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening dir: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing dir: %w", err)
	}
	return nil
}
