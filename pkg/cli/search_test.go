package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchMatchByName(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "search", "hello-gz"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "hello-gz")
	assert.NotContains(t, out, "hello-xz")
}

func TestSearchMatchByControlDescription(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	// "fixture" appears only in the Description field of every fixture, so a
	// match on this term proves the LIKE scan reaches the control column too.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "search", "fixture"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "hello-gz")
	assert.Contains(t, out, "hello-xz")
}

func TestSearchFilteredBySuite(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "search", "fixture", "--suite", "bullseye",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "hello-xz")
	assert.NotContains(t, out, "hello-gz")
}

func TestSearchNoMatchesIsSilent(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "search", "no-such-package"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String())
}

func TestSearchJSONFormat(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "search", "hello", "--format", "json",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		var e listEntry
		require.NoError(t, json.Unmarshal([]byte(line), &e), "bad JSON line: %q", line)
		assert.Contains(t, e.Name, "hello")
	}
}

func TestSearchJSONNoMatchesProducesNoOutput(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	seedTwoSuites(t, root, public)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "search", "no-such-package", "--format", "json",
	}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String())
}

func TestSearchMissingQueryRejected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "search"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr.String(), "query")
}

func TestSearchInvalidFormatRejected(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--repo", root, "search", "hello", "--format", "xml",
	}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.Contains(t, stderr.String(), "format")
}
