package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePoolFile creates publicDir/<rel> (rel is disk-relative, forward-slash)
// with its parent directories and dummy content.
func writePoolFile(t *testing.T, publicDir, rel string) {
	t.Helper()
	abs := filepath.Join(publicDir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte("x"), 0o644))
}

func TestScopePoolDir(t *testing.T) {
	cases := []struct {
		name   string
		distro string
		suite  string
		want   string
	}{
		{"distro only", "debian", "", "debian/pool"},
		{"distro and suite", "debian", "bookworm", "debian/pool/bookworm"},
		{"namespaced distro and suite", "customer/acme", "stable", "customer/acme/pool/stable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, scopePoolDir(tc.distro, tc.suite))
		})
	}
}

func TestScanPoolForOrphans(t *testing.T) {
	publicDir := t.TempDir()

	known := "debian/pool/bookworm/main/h/hello/hello_1.0_amd64.deb"
	orphan := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb"
	bullseyeOrphan := "debian/pool/bullseye/main/g/ghost/ghost_2.0_amd64.deb"
	tmpArtifact := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb.tmp"
	bakArtifact := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb.bak"
	for _, p := range []string{known, orphan, bullseyeOrphan, tmpArtifact, bakArtifact} {
		writePoolFile(t, publicDir, p)
	}
	knownSet := map[string]struct{}{known: {}}

	t.Run("distro scope returns orphans skipping tmp and bak", func(t *testing.T) {
		got, err := scanPoolForOrphans(publicDir, "debian", "", knownSet)
		require.NoError(t, err)
		assert.Equal(t, []string{orphan, bullseyeOrphan}, got)
	})

	t.Run("suite scope narrows the walk", func(t *testing.T) {
		got, err := scanPoolForOrphans(publicDir, "debian", "bookworm", knownSet)
		require.NoError(t, err)
		assert.Equal(t, []string{orphan}, got)
		assert.NotContains(t, got, bullseyeOrphan, "out-of-suite file must not be reported")
	})

	t.Run("nonexistent scope is a no-op", func(t *testing.T) {
		got, err := scanPoolForOrphans(publicDir, "debian", "trixie", knownSet)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("missing distro tree is a no-op", func(t *testing.T) {
		got, err := scanPoolForOrphans(publicDir, "raspbian", "", knownSet)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("everything known yields no orphans", func(t *testing.T) {
		all := map[string]struct{}{known: {}, orphan: {}, bullseyeOrphan: {}}
		got, err := scanPoolForOrphans(publicDir, "debian", "", all)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestScanPoolForOrphansAcrossDistros(t *testing.T) {
	publicDir := t.TempDir()

	debianKnown := "debian/pool/bookworm/main/h/hello/hello_1.0_amd64.deb"
	debianOrphan := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb"
	acmeOrphan := "customer/acme/pool/stable/main/f/foo/foo_1.0_amd64.deb"
	unconfiguredOrphan := "raspbian/pool/bullseye/main/z/zoo/zoo_1.0_armhf.deb"
	for _, p := range []string{debianKnown, debianOrphan, acmeOrphan, unconfiguredOrphan} {
		writePoolFile(t, publicDir, p)
	}
	knownSet := map[string]struct{}{debianKnown: {}}

	got, err := scanPoolForOrphansAcrossDistros(publicDir, []string{"debian", "customer/acme"}, knownSet)
	require.NoError(t, err)
	assert.Equal(t, []string{acmeOrphan, debianOrphan}, got, "union must be sorted")
	assert.NotContains(t, got, unconfiguredOrphan,
		"distros not in the configured list must never be walked")
}

// TestMatchOrphanDistro is a focused unit test for the longest-prefix
// matcher used by removeOrphans. Plain prefixes must not shadow namespaced
// ones; an unmatched path returns "" so the prune is skipped.
func TestMatchOrphanDistro(t *testing.T) {
	distros := []string{"debian", "customer", "customer/acme"}
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "plain distro",
			path: "debian/pool/bookworm/main/h/hello/hello_1.0_amd64.deb",
			want: "debian",
		},
		{
			name: "namespaced distro wins over plain prefix",
			path: "customer/acme/pool/stable/main/f/foo/foo_1.0_amd64.deb",
			want: "customer/acme",
		},
		{
			name: "plain prefix matches when path is not namespaced",
			path: "customer/pool/stable/main/f/foo/foo_1.0_amd64.deb",
			want: "customer",
		},
		{
			name: "no match returns empty",
			path: "raspbian/pool/bullseye/main/f/foo/foo_1.0_armhf.deb",
			want: "",
		},
		{
			name: "missing /pool/ segment returns empty",
			path: "debian/something-else/file.deb",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchOrphanDistro(tc.path, distros))
		})
	}
}

func TestRemoveOrphans(t *testing.T) {
	publicDir := t.TempDir()
	orphan := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb"
	writePoolFile(t, publicDir, orphan)

	var stdout, stderr bytes.Buffer
	err := removeOrphans(publicDir, []string{orphan}, []string{"debian"}, &stdout, &stderr)
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(publicDir, filepath.FromSlash(orphan)))
	assert.NoDirExists(t, filepath.Join(publicDir, "debian", "pool", "bookworm"),
		"emptied suite subtree must be pruned")
	assert.DirExists(t, filepath.Join(publicDir, "debian", "pool"),
		"pool root is the prune stop boundary and must be preserved")
	assert.Equal(t, "removed "+orphan+"\n", stdout.String(),
		"the removed line must print only after the file is actually deleted")
	assert.Empty(t, stderr.String(), "no prune warning expected on the happy path")
}

// TestRemoveOrphansPartialFailureDoesNotOverclaim proves the regression fix:
// when pool.Delete fails mid-list, stdout must reflect only the files that
// were actually removed, never the unreached ones. A `..` segment makes
// pool.Delete (via SafeJoin) reject the second orphan, so the loop returns
// before printing its "removed" line.
func TestRemoveOrphansPartialFailureDoesNotOverclaim(t *testing.T) {
	publicDir := t.TempDir()
	first := "debian/pool/bookworm/main/g/ghost/ghost_1.0_amd64.deb"
	writePoolFile(t, publicDir, first)
	unsafe := "debian/pool/../escape.deb"

	var stdout, stderr bytes.Buffer
	err := removeOrphans(publicDir, []string{first, unsafe}, []string{"debian"}, &stdout, &stderr)

	require.Error(t, err, "an unsafe orphan path must abort the pass")
	assert.NoFileExists(t, filepath.Join(publicDir, filepath.FromSlash(first)),
		"the reachable orphan is still removed before the failure")
	assert.Equal(t, "removed "+first+"\n", stdout.String(),
		"stdout must claim only the orphan actually removed, not the unreached one")
}
