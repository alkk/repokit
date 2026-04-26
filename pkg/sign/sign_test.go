package sign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRelease drops a deterministic Release file into dir and returns its
// absolute path. The body is non-trivial so InRelease assertions can detect
// truncation or off-by-one wrapping bugs.
func writeRelease(t *testing.T, dir string) (path, body string) {
	t.Helper()
	body = "Origin: r\nLabel: r\nSuite: s\nCodename: s\nDate: Sat, 25 Apr 2026 12:00:00 +0000\n"
	path = filepath.Join(dir, "Release")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path, body
}

func TestNopSigner_Sign(t *testing.T) {
	dir := t.TempDir()
	releasePath, body := writeRelease(t, dir)

	require.NoError(t, NopSigner{}.Sign(context.Background(), releasePath))

	inRelease, err := os.ReadFile(filepath.Join(dir, "InRelease"))
	require.NoError(t, err)
	got := string(inRelease)
	assert.True(t, strings.HasPrefix(got, "-----BEGIN PGP SIGNED MESSAGE-----\n"),
		"InRelease should start with the clearsign header marker")
	assert.Contains(t, got, "Hash: SHA256\n\n", "clearsign requires a Hash header followed by a blank line")
	assert.Contains(t, got, body, "Release body must appear verbatim inside InRelease")
	assert.Contains(t, got, "-----BEGIN PGP SIGNATURE-----\n", "InRelease must contain the signature block opener")
	assert.True(t, strings.HasSuffix(got, "-----END PGP SIGNATURE-----\n"),
		"InRelease should end with the signature block closer")

	gpg, err := os.ReadFile(filepath.Join(dir, "Release.gpg"))
	require.NoError(t, err)
	assert.Empty(t, gpg, "NopSigner emits an empty Release.gpg stub")
}

func TestNopSigner_PreservesReleaseBody(t *testing.T) {
	dir := t.TempDir()
	releasePath, body := writeRelease(t, dir)

	require.NoError(t, NopSigner{}.Sign(context.Background(), releasePath))

	onDisk, err := os.ReadFile(releasePath)
	require.NoError(t, err)
	assert.Equal(t, body, string(onDisk), "Sign must not mutate Release in place")
}

func TestNopSigner_MissingReleaseErrors(t *testing.T) {
	dir := t.TempDir()
	err := NopSigner{}.Sign(context.Background(), filepath.Join(dir, "missing"))
	require.Error(t, err)

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no InRelease or Release.gpg should be created when Release is missing")
}

// satisfiesInterface guards against accidental signature drift: NopSigner must
// remain assignable to Signer even after refactors.
func TestNopSigner_SatisfiesSignerInterface(t *testing.T) {
	var s Signer = NopSigner{}
	assert.NotNil(t, s)
}
