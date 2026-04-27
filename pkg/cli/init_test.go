package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/alkk/repokit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitFreshDir(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", root, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	for _, sub := range []string{"pool", "dists"} {
		_, err := os.Stat(filepath.Join(root, sub))
		assert.True(t, os.IsNotExist(err), "%s/ should not exist after init", sub)
	}
	dbInfo, err := os.Stat(filepath.Join(root, dbFilename))
	require.NoError(t, err)
	assert.False(t, dbInfo.IsDir())
	assert.Positive(t, dbInfo.Size(), "repo.db should be non-empty after migrations")

	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "REPLACE_ME", cfg.SigningKey)
	assert.Empty(t, cfg.Suites)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{dbFilename, "config.yaml"}, names,
		"init should leave only repo.db and config.yaml at the root")

	assert.Contains(t, stdout.String(), "initialized repo at")
}

func TestInitErrorsIfRepoDBExists(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, dbFilename), nil, 0o600))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "already initialized")
}

func TestInitPreservesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	custom := []byte("signing_key: \"USER_KEY\"\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), custom, 0o600))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	got, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, custom, got, "init should not overwrite an existing config.yaml")
}

func TestInitWithPositional(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "explicit")
	t.Setenv(envRepo, "")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"init", root}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())

	_, err := os.Stat(filepath.Join(root, dbFilename))
	require.NoError(t, err)
	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "REPLACE_ME", cfg.SigningKey)
}

func TestInitConfigTemplateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configTemplate), 0o600))

	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.SigningKey)

	got := cfg.SuiteOrDefault("debian", "bookworm")
	assert.Equal(t, "debian", got.Origin)
	assert.Equal(t, "bookworm", got.Codename)
}

func TestInitNonInitCommandRequiresRepoDB(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(envRepo, "")
	t.Chdir(empty)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"list"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "no repo.db")
}
