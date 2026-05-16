package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alkk/repokit/pkg/pool"
)

// poolDirName is the literal "pool" segment that sits under each per-distro
// public-tree root (i.e. <publicDir>/<distro>/pool/...). It is not a
// top-level directory under <publicDir> either — every pool file lives
// under a per-distro subdirectory.
const poolDirName = "pool"

// scanPoolForOrphans walks the per-distro pool subtree narrowed by suite and
// returns the sorted disk-relative paths (<distro>/pool/...) of every regular
// file not present in the supplied DB set. The walk is a no-op (nil slice)
// when the scoped directory does not exist (e.g. a distro with no published
// files yet). distro must be non-empty; the unscoped case is handled by
// scanPoolForOrphansAcrossDistros.
func scanPoolForOrphans(publicDir, distro, suite string, known map[string]struct{}) ([]string, error) {
	rel := scopePoolDir(distro, suite)
	abs := filepath.Join(publicDir, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", rel, err)
	}
	var orphans []string
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// .tmp and .bak are transient artifacts of an in-flight or crashed
		// pool write — see pool.Replace. Treating them as orphans would let
		// gc destroy the only recoverable copy after an interrupted
		// Replace; they are cleaned up by the next successful Replace at
		// the same path instead.
		if strings.HasSuffix(d.Name(), ".tmp") || strings.HasSuffix(d.Name(), ".bak") {
			return nil
		}
		relPath, rerr := filepath.Rel(publicDir, p)
		if rerr != nil {
			return rerr //nolint:wrapcheck // surfaced verbatim by the outer wrap
		}
		key := filepath.ToSlash(relPath)
		if _, ok := known[key]; ok {
			return nil
		}
		orphans = append(orphans, key)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walking %s: %w", rel, walkErr)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// scanPoolForOrphansAcrossDistros walks <publicDir>/<distro>/pool for every
// configured distro and returns the sorted union of orphans. Used when the
// caller runs unscoped — the union of all per-distro pools is the search
// space; repokit never walks distros the operator has not declared.
func scanPoolForOrphansAcrossDistros(publicDir string, distros []string, known map[string]struct{}) ([]string, error) {
	var orphans []string
	for _, d := range distros {
		found, err := scanPoolForOrphans(publicDir, d, "", known)
		if err != nil {
			return nil, err
		}
		orphans = append(orphans, found...)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// scopePoolDir returns the disk-relative subtree to walk for orphan detection
// inside a single distro. The unscoped (no distro) case has no single root —
// callers iterate over distros and call this per-distro.
func scopePoolDir(distro, suite string) string {
	if suite != "" {
		return distro + "/" + poolDirName + "/" + suite
	}
	return distro + "/" + poolDirName
}

// removeOrphans deletes the listed pool files and prunes the now-empty
// ancestor directories up to (but not including) <publicDir>/<distro>/pool.
// pool.Delete is idempotent so a second call would be a no-op; we still
// surface real I/O errors. The "removed <path>" line is written only after
// that file's pool.Delete succeeds, so a mid-list I/O failure does not leave
// stdout falsely claiming the unreached orphans were reclaimed. Prune errors
// are non-fatal (the file is gone, the DB row is gone, a leftover empty dir
// is cosmetic) and are warned to stderr.
func removeOrphans(publicDir string, orphans, distros []string, stdout, stderr io.Writer) error {
	for _, p := range orphans {
		if err := pool.Delete(publicDir, p); err != nil {
			return fmt.Errorf("removing orphan %s: %w", p, err)
		}
		fmt.Fprintf(stdout, "removed %s\n", p)
		distro := matchOrphanDistro(p, distros)
		if distro == "" {
			continue
		}
		if err := pool.PruneEmptyParents(publicDir, distro, p); err != nil {
			fmt.Fprintf(stderr, "prune: %v\n", err)
		}
	}
	return nil
}

// matchOrphanDistro returns the configured distro whose <distro>/pool/
// prefix matches p, or "" if none does. Picks the longest match so a plain
// "customer" never shadows a namespaced "customer/acme" — required because
// namespaced distros (`customer/acme`) contain a `/` and a naive
// strings.Cut on the first `/` would mis-anchor the prune stop boundary at
// <publicDir>/customer/pool, potentially letting the climb remove
// <publicDir>/customer/acme/pool itself. The config loader already rejects
// a plain-prefix collision like [customer, customer/acme]; the longest-
// match logic here is defense in depth.
func matchOrphanDistro(p string, distros []string) string {
	best := ""
	for _, d := range distros {
		prefix := d + "/" + poolDirName + "/"
		if strings.HasPrefix(p, prefix) && len(d) > len(best) {
			best = d
		}
	}
	return best
}
