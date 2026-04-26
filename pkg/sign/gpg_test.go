package sign

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGPG installs a fake gpg shim on PATH that records every invocation's
// argv (one line per argument) into a log file under dir. The shim writes a
// deterministic body to the path given to --output and exits with the supplied
// status. Returns the log file path so the test can read what gpg was called
// with.
func stubGPG(t *testing.T, exitStatus int, outputBody string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("gpg shim relies on a POSIX shell")
	}
	bin := t.TempDir()
	logPath := filepath.Join(bin, "gpg.log")

	script := `#!/bin/sh
log='` + logPath + `'
out=''
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$log"
  if [ "$capture" = "1" ]; then
    out="$arg"
    capture=0
  fi
  if [ "$arg" = "--output" ]; then
    capture=1
  fi
done
if [ -n "$out" ]; then
  printf '%s' '` + outputBody + `' > "$out"
fi
exit ` + strconv.Itoa(exitStatus) + `
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gpg"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// readArgvLog returns the gpg invocation argv lists in invocation order.
// Each invocation's argv is a slice; consecutive invocations are split by
// the appearance of "--batch" (the first flag in every gpg call).
func readArgvLog(t *testing.T, path string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	var calls [][]string
	cur := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "--batch" && len(cur) > 0 {
			calls = append(calls, cur)
			cur = nil
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		calls = append(calls, cur)
	}
	return calls
}

func writeReleaseInDir(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "Release")
	require.NoError(t, os.WriteFile(path, []byte("Origin: r\nSuite: s\n"), 0o644))
	return path
}

func TestGPGSigner_InvokesClearsignAndDetachSign(t *testing.T) {
	logPath := stubGPG(t, 0, "SIGNED")
	dir := t.TempDir()
	releasePath := writeReleaseInDir(t, dir)

	require.NoError(t, GPGSigner{KeyID: "DEADBEEF"}.Sign(context.Background(), releasePath))

	calls := readArgvLog(t, logPath)
	require.Len(t, calls, 2, "Sign must invoke gpg exactly twice")

	clearsign := calls[0]
	assert.Equal(t, []string{
		"--batch", "--yes",
		"--default-key", "DEADBEEF",
		"--digest-algo", "SHA256",
		"--clearsign",
		"--output", filepath.Join(dir, "InRelease"),
		releasePath,
	}, clearsign)

	detach := calls[1]
	assert.Equal(t, []string{
		"--batch", "--yes",
		"--default-key", "DEADBEEF",
		"--digest-algo", "SHA256",
		"--armor", "--detach-sign",
		"--output", filepath.Join(dir, "Release.gpg"),
		releasePath,
	}, detach)

	for _, name := range []string{"InRelease", "Release.gpg"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, "SIGNED", string(body), "%s should contain stub gpg output", name)
	}
}

func TestGPGSigner_PropagatesGPGFailure(t *testing.T) {
	stubGPG(t, 2, "")
	dir := t.TempDir()
	releasePath := writeReleaseInDir(t, dir)

	err := GPGSigner{KeyID: "DEADBEEF"}.Sign(context.Background(), releasePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clearsigning Release")
}

func TestGPGSigner_MissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH semantics differ on Windows")
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	dir := t.TempDir()
	releasePath := writeReleaseInDir(t, dir)

	err := GPGSigner{KeyID: "DEADBEEF"}.Sign(context.Background(), releasePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpg")
}

func TestGPGSigner_EmptyKeyID(t *testing.T) {
	dir := t.TempDir()
	releasePath := writeReleaseInDir(t, dir)

	err := GPGSigner{KeyID: ""}.Sign(context.Background(), releasePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KeyID is empty")
}

func TestGPGSigner_SatisfiesSignerInterface(t *testing.T) {
	var s Signer = GPGSigner{KeyID: "x"}
	assert.NotNil(t, s)
}
