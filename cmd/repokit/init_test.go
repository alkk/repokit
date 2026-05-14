package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/alkk/repokit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withDefaultStateDir swaps the package-level defaultStateDir for the duration
// of a test so resolution does not touch the real ~/.repokit/.
func withDefaultStateDir(t *testing.T, path string) {
	t.Helper()
	prev := defaultStateDir
	defaultStateDir = func() (string, error) { return path, nil }
	t.Cleanup(func() { defaultStateDir = prev })
}

func TestInitFreshDir(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

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
	code := run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "already initialized")
}

func TestInitPreservesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	custom := []byte("signing_key: \"USER_KEY\"\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), custom, 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

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
	code := run([]string{"init", root}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

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
	withDefaultStateDir(t, empty)

	var stdout, stderr bytes.Buffer
	code := run([]string{"list"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "no repo.db")
}

func TestInitDefaultsToHomeDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), ".repokit")
	t.Setenv(envRepo, "")
	withDefaultStateDir(t, stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	_, err := os.Stat(filepath.Join(stateDir, dbFilename))
	require.NoError(t, err)
	cfg, err := config.Load(filepath.Join(stateDir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "REPLACE_ME", cfg.SigningKey)
	assert.Equal(t, "REPLACE_ME", cfg.PublicDir)
}

func TestInitPositionalBeatsDefault(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), "default")
	explicit := filepath.Join(t.TempDir(), "explicit")
	t.Setenv(envRepo, "")
	withDefaultStateDir(t, defaultDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", explicit}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	_, err := os.Stat(filepath.Join(explicit, dbFilename))
	require.NoError(t, err, "positional path should be initialized")
	_, err = os.Stat(defaultDir)
	assert.True(t, os.IsNotExist(err), "default dir should not be touched when positional given")
}

func TestInitFlagBeatsDefault(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), "default")
	custom := filepath.Join(t.TempDir(), "custom")
	t.Setenv(envRepo, "")
	withDefaultStateDir(t, defaultDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", custom, "init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	_, err := os.Stat(filepath.Join(custom, dbFilename))
	require.NoError(t, err)
	_, err = os.Stat(defaultDir)
	assert.True(t, os.IsNotExist(err), "default dir should not be touched when --repo given")
}

// TestRunInitWithPositionalWithoutHomeDir guards against init regressing to
// require a resolvable home directory when the operator supplied a positional
// path. resolveRepoRoot fails on defaultStateDir before init can use the
// override otherwise — minimal containers / CI sandboxes without $HOME.
func TestRunInitWithPositionalWithoutHomeDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "explicit")
	t.Setenv(envRepo, "")
	prev := defaultStateDir
	defaultStateDir = func() (string, error) { return "", assert.AnError }
	t.Cleanup(func() { defaultStateDir = prev })

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", root}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())

	_, err := os.Stat(filepath.Join(root, dbFilename))
	require.NoError(t, err)
}

// TestRunInitWithoutPositionalWithoutHomeDir guards the inverse: when neither
// a positional path nor --repo / $REPOKIT_ROOT is set and home-dir resolution
// fails, init must surface a clear error instead of silently calling
// MkdirAll("") and producing a confusing low-level message.
func TestRunInitWithoutPositionalWithoutHomeDir(t *testing.T) {
	t.Setenv(envRepo, "")
	prev := defaultStateDir
	defaultStateDir = func() (string, error) { return "", assert.AnError }
	t.Cleanup(func() { defaultStateDir = prev })

	var stdout, stderr bytes.Buffer
	code := run([]string{"init"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "no repo root resolved")
}

func TestConfigTemplateHasPublicDirPlaceholder(t *testing.T) {
	assert.Contains(t, configTemplate, `public_dir: "REPLACE_ME"`,
		"configTemplate must include a public_dir REPLACE_ME placeholder")
}

func TestConfigTemplateHasDistros(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configTemplate), 0o600))

	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Distros, "seed config must include a non-empty distros list")
	for _, d := range cfg.Distros {
		assert.Regexp(t, `^[a-zA-Z0-9-]+$`, d, "seed distro %q must satisfy validateDistroName", d)
	}
	assert.Contains(t, cfg.Distros, "debian")
	assert.Contains(t, cfg.Distros, "ubuntu")
}
