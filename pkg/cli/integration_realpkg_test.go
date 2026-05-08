package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realPkg returns the absolute path of a .deb under test-packages/. Tests
// run with cwd == pkg/cli, so test-packages/ is two levels up from cwd.
func realPkg(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "test-packages", rel))
	require.NoError(t, err)
	require.FileExists(t, abs)
	return abs
}

// findPoolFile walks <root>/<distro>/pool/ and returns the first repo-root
// relative path whose basename equals name, or "" when not found.
func findPoolFile(t *testing.T, root, distro, name string) string {
	t.Helper()
	var hit string
	err := filepath.WalkDir(filepath.Join(root, distro, "pool"),
		func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() == name {
				rel, rerr := filepath.Rel(root, p)
				if rerr != nil {
					return rerr //nolint:wrapcheck // surfaced verbatim to caller's require.NoError
				}
				hit = filepath.ToSlash(rel)
				return fs.SkipAll
			}
			return nil
		})
	require.NoError(t, err)
	return hit
}

func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// TestIntegration_RealPackages exercises the add/publish pipeline against
// realistically-built .deb packages: full control fields (Maintainer, Section,
// Priority, Description, Depends), real compressed control.tar, multiple
// versions of the same package, an arch=all binary, and three binary outputs
// produced from one source so pool sharding routes them under the Source
// shard rather than each binary's own first letter.
func TestIntegration_RealPackages(t *testing.T) {
	root := t.TempDir()

	runCmd := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run(append([]string{"--repo", root, name}, args...),
			&stdout, &stderr, "test-rev")
		require.Equalf(t, ExitOK, code, "%s failed: %s", name, stderr.String())
	}

	runCmd("init")

	// Three versions × two arches of the simple per-arch package.
	versions := []string{"1.0-1", "1.0-2", "1.1-1"}
	arches := []string{"amd64", "arm64"}
	for _, ver := range versions {
		for _, arch := range arches {
			deb := realPkg(t, "repokit-test-package/out/"+ver+"/"+arch+
				"/repokit-test-package_"+ver+"_"+arch+".deb")
			runCmd("add", "--distro", "debian", "--suite", "bookworm", deb)
		}
	}

	// One arch=all binary. Picking the xz variant — the gzip/zstd siblings
	// only differ in the *internal* tarball compression and round-trip the
	// same once parsed; pkg/deb already covers all three at the unit level.
	runCmd("add", "--distro", "debian", "--suite", "bookworm",
		realPkg(t, "repokit-test-package-all/out/1.0-1/xz/"+
			"repokit-test-package-all_1.0-1_all.deb"))

	// Multi-binary source: three .debs from one source package.
	// multibin-common is arch=all, the other two are arch=amd64. The two
	// non-source-named binaries declare `Source: repokit-test-multibin`,
	// so all three must shard under r/repokit-test-multibin/ rather than
	// each one's own first letter.
	multibinDir := "repokit-test-multibin/out/1_1.0-1/amd64/xz/"
	multibinDebs := []string{
		multibinDir + "repokit-test-multibin_1.0-1_amd64.deb",
		multibinDir + "repokit-test-multibin-common_1.0-1_all.deb",
		multibinDir + "repokit-test-multibin-dev_1.0-1_amd64.deb",
	}
	addArgs := make([]string, 0, 5+len(multibinDebs))
	addArgs = append(addArgs, "add", "--distro", "debian", "--suite", "bookworm")
	for _, p := range multibinDebs {
		addArgs = append(addArgs, realPkg(t, p))
	}
	runCmd(addArgs[0], addArgs[1:]...)

	runCmd("publish", "--no-sign")

	suiteDir := filepath.Join(root, "debian", "dists", "bookworm")

	t.Run("multi_version_per_arch", func(t *testing.T) {
		for _, arch := range arches {
			pkgPath := filepath.Join(suiteDir, "main", "binary-"+arch, "Packages")
			stanzas := readStanzas(t, pkgPath)
			for _, ver := range versions {
				assert.Truef(t, hasStanza(stanzas, "repokit-test-package", ver, arch),
					"binary-%s/Packages should contain repokit-test-package %s",
					arch, ver)
			}
		}
	})

	t.Run("arch_all_fans_out_to_concrete_arches", func(t *testing.T) {
		for _, arch := range arches {
			pkgPath := filepath.Join(suiteDir, "main", "binary-"+arch, "Packages")
			stanzas := readStanzas(t, pkgPath)
			assert.Truef(t, hasStanza(stanzas, "repokit-test-package-all", "1.0-1", "all"),
				"arch=all stanza missing from binary-%s/Packages", arch)
			// multibin-common is also arch=all and must fan out the same way.
			assert.Truef(t, hasStanza(stanzas, "repokit-test-multibin-common", "1:1.0-1", "all"),
				"multibin-common arch=all missing from binary-%s/Packages", arch)
		}
		_, err := os.Stat(filepath.Join(suiteDir, "main", "binary-all"))
		require.ErrorIs(t, err, fs.ErrNotExist,
			"no binary-all directory must be created for arch=all rows")
	})

	t.Run("multibin_source_shard", func(t *testing.T) {
		// multibin's Version field carries an epoch (`1:1.0-1`). Repokit
		// keeps the epoch in the pool filename (deliberately — diverges from
		// Debian convention) so two builds that differ only by epoch on the
		// same upstream version+revision do not collide in the pool.
		for _, name := range []string{
			"repokit-test-multibin_1:1.0-1_amd64.deb",
			"repokit-test-multibin-common_1:1.0-1_all.deb",
			"repokit-test-multibin-dev_1:1.0-1_amd64.deb",
		} {
			got := findPoolFile(t, root, "debian", name)
			require.NotEmptyf(t, got, "%s missing from pool", name)
			assert.Containsf(t, got, "/main/r/repokit-test-multibin/",
				"%s should shard under the source name (got %s)", name, got)
		}
	})

	t.Run("pool_files_byte_identical_to_source", func(t *testing.T) {
		// One per-arch binary plus one arch=all keeps the assertion cheap
		// while still proving the pool file is a verbatim copy of the .deb.
		check := []struct {
			src      string
			poolName string
		}{
			{
				src: realPkg(t, "repokit-test-package/out/1.1-1/amd64/"+
					"repokit-test-package_1.1-1_amd64.deb"),
				poolName: "repokit-test-package_1.1-1_amd64.deb",
			},
			{
				src: realPkg(t, "repokit-test-package-all/out/1.0-1/xz/"+
					"repokit-test-package-all_1.0-1_all.deb"),
				poolName: "repokit-test-package-all_1.0-1_all.deb",
			},
		}
		for _, tc := range check {
			poolRel := findPoolFile(t, root, "debian", tc.poolName)
			require.NotEmpty(t, poolRel, tc.poolName)
			assert.Equalf(t, sha256Hex(t, tc.src),
				sha256Hex(t, filepath.Join(root, filepath.FromSlash(poolRel))),
				"pool file %s must be byte-identical to %s", tc.poolName, tc.src)
		}
	})

	t.Run("control_fields_preserved", func(t *testing.T) {
		// The multibin .deb declares Maintainer, Section, Priority, Depends,
		// and a multi-line Description — all should round-trip into Packages
		// verbatim. The synthetic-deb test can't easily cover this because
		// buildTestDeb in integration_test.go emits a stripped-down control.
		body, err := os.ReadFile(filepath.Join(suiteDir, "main", "binary-amd64", "Packages"))
		require.NoError(t, err)
		stanzas := splitPackagesStanzas(body)

		var stanza string
		for _, s := range stanzas {
			if strings.Contains(s, "Package: repokit-test-multibin\n") &&
				strings.Contains(s, "Architecture: amd64\n") {
				stanza = s
				break
			}
		}
		require.NotEmpty(t, stanza, "repokit-test-multibin amd64 stanza missing")

		for _, want := range []string{
			"Maintainer: repokit test <test@example.com>",
			"Section: misc",
			"Priority: optional",
			"Depends: repokit-test-multibin-common (= 1:1.0-1)",
			"Description: repokit test multibin (binary)",
			"\n Architecture-specific binary output for repo indexer tests.",
		} {
			assert.Containsf(t, stanza, want, "stanza missing %q", want)
		}
	})
}
