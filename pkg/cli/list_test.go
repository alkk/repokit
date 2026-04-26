package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTwoSuites adds gz fixture into bookworm and xz fixture into bullseye so
// list/search tests have two distinct rows to filter against.
func seedTwoSuites(t *testing.T, root string) {
	t.Helper()
	for _, tc := range []struct {
		suite string
		deb   string
	}{
		{"bookworm", fixtureDeb(t, "hello-gz_1.0_amd64.deb")},
		{"bullseye", fixtureDeb(t, "hello-xz_1.0_amd64.deb")},
	} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"--repo", root, "add",
			"--distro", "debian", "--suite", tc.suite,
			tc.deb,
		}, &stdout, &stderr, "test-rev")
		require.Equal(t, ExitOK, code, "seeding %s failed: %s", tc.suite, stderr.String())
	}
}

func TestListTableShowsHeaderAndRows(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "list"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 3, "expected header + two rows, got: %q", out)
	assert.Contains(t, lines[0], "DISTRO")
	assert.Contains(t, lines[0], "VERSION")
	assert.Contains(t, lines[0], "SIZE")
	// Rows ordered by (distro, suite, ...): bookworm before bullseye.
	assert.Contains(t, lines[1], "hello-gz")
	assert.Contains(t, lines[1], "bookworm")
	assert.Contains(t, lines[2], "hello-xz")
	assert.Contains(t, lines[2], "bullseye")
}

func TestListFiltersBySuite(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "list", "--suite", "bullseye",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "hello-xz")
	assert.NotContains(t, out, "hello-gz")
}

func TestListFiltersByName(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "list", "--name", "hello-gz",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "hello-gz")
	assert.NotContains(t, out, "hello-xz")
}

func TestListEmptyTableSilent(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "list"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String(), "empty table output should be silent")
}

func TestListJSONFormat(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "list", "--format", "json",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	require.Len(t, lines, 2)

	entries := make([]listEntry, 0, len(lines))
	for _, line := range lines {
		var e listEntry
		require.NoError(t, json.Unmarshal([]byte(line), &e), "bad JSON line: %q", line)
		entries = append(entries, e)
	}
	assert.Equal(t, "hello-gz", entries[0].Name)
	assert.Equal(t, "bookworm", entries[0].Suite)
	assert.NotEmpty(t, entries[0].SHA256)
	assert.Positive(t, entries[0].Size)
	assert.Equal(t, "hello-xz", entries[1].Name)
	assert.Equal(t, "bullseye", entries[1].Suite)
}

func TestListJSONEmptyProducesNoOutput(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "list", "--format", "json",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String())
}

func TestListInvalidFormatRejected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "list", "--format", "xml",
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr.String(), "format")
}
