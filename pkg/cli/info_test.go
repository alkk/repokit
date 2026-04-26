package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfoPrintsControlAndHashes(t *testing.T) {
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	// info should not require a repo — run from a clean cwd with no DEBREPO_ROOT.
	t.Setenv(envRepo, "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"info", deb}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "Package:      hello-gz")
	assert.Contains(t, out, "Version:      1.0")
	assert.Contains(t, out, "Architecture: amd64")
	assert.Contains(t, out, "MD5:")
	assert.Contains(t, out, "SHA1:")
	assert.Contains(t, out, "SHA256:")
	assert.Contains(t, out, "SHA512:")
	assert.Contains(t, out, "Size:")
	assert.Contains(t, out, "Pool path:")
	assert.Contains(t, out, "(provide --distro and --suite to compute)")
}

func TestInfoComputesPoolPathWhenScopeProvided(t *testing.T) {
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	t.Setenv(envRepo, "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"info",
		"--distro", "debian", "--suite", "bookworm", "--component", "main",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "Pool path:    pool/debian/bookworm/main/h/hello-gz/hello-gz_1.0_amd64.deb")
}

func TestInfoExposesParsedSource(t *testing.T) {
	// hello-zst fixture uses "Source: foo (1.2-3)" so we can verify both bare
	// source extraction and the resulting pool-path shard.
	deb := fixtureDeb(t, "hello-zst_1.0_amd64.deb")

	t.Setenv(envRepo, "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"info",
		"--distro", "debian", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "Source:       foo")
	assert.Contains(t, out, "Pool path:    pool/debian/bookworm/main/f/foo/hello-zst_1.0_amd64.deb")
}

func TestInfoMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.deb")

	t.Setenv(envRepo, "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{"info", missing}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "info:")
}

func TestInfoRejectsUnsafeScope(t *testing.T) {
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	t.Setenv(envRepo, "")
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"info",
		"--distro", "../etc", "--suite", "bookworm",
		deb,
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "--distro")
}

func TestInfoDoesNotRequireRepo(t *testing.T) {
	// resolve fixture absolute path before chdir, since fixtureDeb is cwd-relative.
	deb := fixtureDeb(t, "hello-xz_1.0_amd64.deb")

	empty := t.TempDir()
	t.Setenv(envRepo, "")
	t.Chdir(empty)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"info", deb}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "Package:      hello-xz")
}
