package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	t.Run("valid config loads with explicit and defaulted suites", func(t *testing.T) {
		c, err := Load("testdata/valid.yaml")
		require.NoError(t, err)
		assert.Equal(t, "DEADBEEF1234", c.SigningKey)

		bookworm, ok := c.Suites["debian/bookworm"]
		require.True(t, ok)
		assert.Equal(t, SuiteConfig{
			Origin:      "MyRepo",
			Label:       "MyRepo",
			Suite:       "bookworm",
			Codename:    "bookworm",
			Description: "Internal builds for Debian 12",
		}, bookworm)

		bullseye, ok := c.Suites["debian/bullseye"]
		require.True(t, ok)
		assert.Equal(t, "MyRepo", bullseye.Origin)
		assert.Equal(t, "MyRepo", bullseye.Label)
		assert.Equal(t, "bullseye", bullseye.Suite, "missing suite name should default from key")
		assert.Equal(t, "bullseye", bullseye.Codename, "missing codename should default from key")
		assert.Equal(t, "debian bullseye", bullseye.Description, "missing description should default")
	})

	t.Run("missing signing_key is accepted at load time", func(t *testing.T) {
		// publish enforces signing_key when --no-sign is absent (via pickSigner);
		// add/delete/verify never need it. config.Load must not gate on it or
		// commands that don't sign would regress when config.yaml omits the key.
		c, err := Load("testdata/invalid_missing_key.yaml")
		require.NoError(t, err)
		assert.Empty(t, c.SigningKey)
	})

	t.Run("nonexistent file returns wrapped error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("malformed yaml returns parse error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: [unterminated\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing")
	})

	t.Run("blank signing_key is accepted at load time", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "blank.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: \"   \"\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, "   ", c.SigningKey)
	})

	t.Run("signing_key with newline is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nl.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: \"KEY\\nExtra-Header: x\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "newline")
	})

	t.Run("signing_key starting with dash is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dash.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: \"--evil-flag\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not start with '-'")
	})

	t.Run("malformed suite key is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "badkey.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: KEY\nsuites:\n  noslash:\n    origin: x\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "<distro>/<suite>")
	})

	t.Run("suite key with extra slash is rejected", func(t *testing.T) {
		// SuiteOrDefault looks up exactly "<distro>/<suite>" and the CLI
		// rejects "/" inside --suite, so a key like "debian/bookworm/security"
		// is unreachable. Reject it at load time rather than silently
		// accepting a dead config entry.
		path := filepath.Join(t.TempDir(), "extraslash.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\nsuites:\n  debian/bookworm/security:\n    origin: x\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "<distro>/<suite>")
	})

	t.Run("valid public_dir is parsed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pubdir.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\npublic_dir: \"/srv/repokit\"\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, "/srv/repokit", c.PublicDir)
	})

	t.Run("empty public_dir is accepted at load time", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\npublic_dir: \"\"\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Empty(t, c.PublicDir)
	})

	t.Run("public_dir with newline is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pdnl.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\npublic_dir: \"/srv\\nInjected: y\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir")
		assert.Contains(t, err.Error(), "control")
	})

	t.Run("public_dir with NUL is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pdnul.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\npublic_dir: \"/srv\\0evil\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir")
		assert.Contains(t, err.Error(), "control")
	})

	t.Run("public_dir with control char is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pdctl.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\npublic_dir: \"/srv\\u0007bell\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir")
		assert.Contains(t, err.Error(), "control")
	})

	t.Run("suite header newline is rejected per field", func(t *testing.T) {
		fields := []string{"origin", "label", "suite", "codename", "description"}
		for _, field := range fields {
			t.Run(field, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), field+".yaml")
				body := fmt.Sprintf(
					"signing_key: KEY\nsuites:\n  debian/bookworm:\n    %s: \"x\\nInjected: y\"\n",
					field,
				)
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

				_, err := Load(path)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "newline")
				assert.Contains(t, err.Error(), field)
			})
		}
	})
}

func TestSuiteOrDefault(t *testing.T) {
	t.Run("returns configured block when present", func(t *testing.T) {
		c := &Config{
			SigningKey: "k",
			Suites: map[string]SuiteConfig{
				"debian/bookworm": {Origin: "Acme", Label: "Acme", Suite: "bookworm", Codename: "bookworm", Description: "d"},
			},
		}
		got := c.SuiteOrDefault("debian", "bookworm")
		assert.Equal(t, "Acme", got.Origin)
		assert.Equal(t, "d", got.Description)
	})

	t.Run("synthesizes default when suite is unknown", func(t *testing.T) {
		c := &Config{SigningKey: "k"}
		got := c.SuiteOrDefault("ubuntu", "noble")
		assert.Equal(t, SuiteConfig{
			Origin:      "ubuntu",
			Label:       "ubuntu",
			Suite:       "noble",
			Codename:    "noble",
			Description: "ubuntu noble",
		}, got)
	})

	t.Run("nil receiver returns synthesized default", func(t *testing.T) {
		var c *Config
		got := c.SuiteOrDefault("debian", "trixie")
		assert.Equal(t, "trixie", got.Suite)
		assert.Equal(t, "debian", got.Origin)
	})
}
