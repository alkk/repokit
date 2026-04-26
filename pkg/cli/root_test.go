package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRepoRoot(t *testing.T) {
	t.Run("flag wins over env and cwd", func(t *testing.T) {
		flagDir := t.TempDir()
		envDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(flagDir, dbFilename), nil, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(envDir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, envDir)

		root, err := resolveRepoRoot(flagDir, "list")
		require.NoError(t, err)
		assert.Equal(t, flagDir, root)
	})

	t.Run("env wins over cwd", func(t *testing.T) {
		envDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(envDir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, envDir)

		root, err := resolveRepoRoot("", "list")
		require.NoError(t, err)
		assert.Equal(t, envDir, root)
	})

	t.Run("cwd fallback requires repo.db", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv(envRepo, "")
		t.Chdir(empty)

		_, err := resolveRepoRoot("", "list")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrRepoNotFound)
	})

	t.Run("cwd fallback succeeds when repo.db present", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, "")
		t.Chdir(dir)

		root, err := resolveRepoRoot("", "list")
		require.NoError(t, err)
		// On macOS the temp dir is symlinked through /private; compare resolved paths.
		got, _ := filepath.EvalSymlinks(root)
		want, _ := filepath.EvalSymlinks(dir)
		assert.Equal(t, want, got)
	})

	t.Run("init does not require repo.db", func(t *testing.T) {
		empty := t.TempDir()

		root, err := resolveRepoRoot(empty, "init")
		require.NoError(t, err)
		assert.Equal(t, empty, root)
	})

	t.Run("missing repo.db returns ErrRepoNotFound", func(t *testing.T) {
		empty := t.TempDir()

		_, err := resolveRepoRoot(empty, "list")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrRepoNotFound)
	})
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--help"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitOK, code)
	assert.Contains(t, stdout.String(), "Usage:")
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--definitely-not-a-flag"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.NotEmpty(t, stderr.String())
}

func TestRunInitWithoutDB(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.NotContains(t, stderr.String(), "no repo.db")
}

func TestRunMissingRepoDB(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--repo", dir, "list"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitError, code)
	assert.Contains(t, stderr.String(), "no repo.db")
}

func TestRunNoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(nil, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
}

func TestValidateScopeName(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		val     string
		wantErr bool
	}{
		{"empty allowed", "distro", "", false},
		{"plain name", "suite", "bookworm", false},
		{"dot segment", "suite", "..", true},
		{"slash forbidden", "distro", "etc/passwd", true},
		{"backslash forbidden", "suite", "a\\b", true},
		{"leading dot", "distro", ".hidden", true},
		{"leading dash", "arch", "-amd64", true},
		{"suite .new suffix", "suite", "bookworm.new", true},
		{"suite .old suffix", "suite", "bookworm.old", true},
		{"distro .old suffix allowed", "distro", "debian.old", false},
		{"component .new suffix allowed", "component", "main.new", false},
		{"newline rejected", "suite", "bookworm\nLabel: Forged", true},
		{"carriage return rejected", "distro", "debian\r", true},
		{"tab rejected", "component", "main\t", true},
		{"del rejected", "arch", "amd64\x7f", true},
		{"space rejected", "suite", "my suite", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScopeName(tc.field, tc.val)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestRequireScopeName(t *testing.T) {
	t.Run("empty rejected", func(t *testing.T) {
		err := requireScopeName("distro", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
	})
	t.Run("non-empty delegates to validateScopeName", func(t *testing.T) {
		require.NoError(t, requireScopeName("distro", "debian"))
		require.Error(t, requireScopeName("suite", "etc/passwd"))
	})
}

