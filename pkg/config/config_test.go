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

	t.Run("namespaced suite key parses with last-slash split", func(t *testing.T) {
		// splitSuiteKey uses LastIndex('/') so a namespaced distro like
		// "customer/acme" combined with suite "stable" yields key
		// "customer/acme/stable" and parses to ("customer/acme", "stable").
		path := filepath.Join(t.TempDir(), "nsuite.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - customer/acme\nsuites:\n  customer/acme/stable:\n    origin: Acme\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		s, ok := c.Suites["customer/acme/stable"]
		require.True(t, ok)
		assert.Equal(t, "Acme", s.Origin)
		// defaults fill in for Suite/Codename from the right half
		assert.Equal(t, "stable", s.Suite)
		assert.Equal(t, "stable", s.Codename)
		// SuiteOrDefault round-trips against the loaded config
		got := c.SuiteOrDefault("customer/acme", "stable")
		assert.Equal(t, "Acme", got.Origin)
		// default Label falls back to the namespaced distro verbatim
		assert.Equal(t, "customer/acme", got.Label)
	})

	t.Run("suite key with too-deep distro is rejected by distro validator", func(t *testing.T) {
		// With last-'/' split, "customer/acme/stable/extra" parses to
		// ("customer/acme/stable", "extra") and the left half then fails
		// ValidateDistroName because it contains two '/' separators.
		path := filepath.Join(t.TempDir(), "toodeep.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - customer/acme\nsuites:\n  customer/acme/stable/extra:\n    origin: x\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "customer/acme/stable")
		assert.Contains(t, err.Error(), "single segment or two segments")
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
					"signing_key: KEY\ndistros:\n  - debian\nsuites:\n  debian/bookworm:\n    %s: \"x\\nInjected: y\"\n",
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

	t.Run("distros entries are parsed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "distros.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - debian\n  - ubuntu\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, []string{"debian", "ubuntu"}, c.Distros)
	})

	t.Run("empty distros is accepted at load time", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nodistros.yaml")
		require.NoError(t, os.WriteFile(path, []byte("signing_key: KEY\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Empty(t, c.Distros)
	})

	t.Run("duplicate distros entry is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dup.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - debian\n  - debian\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
		assert.Contains(t, err.Error(), "debian")
	})

	t.Run("invalid distros entry is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - \"deb ian\"\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deb ian")
	})

	t.Run("namespaced distro conflicts with plain prefix is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conflict.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - customer\n  - customer/acme\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "customer/acme")
		assert.Contains(t, err.Error(), "customer")
	})

	t.Run("namespaced before plain prefix is rejected (order independent)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conflict-rev.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - customer/acme\n  - customer\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "customer/acme")
		assert.Contains(t, err.Error(), "customer")
	})

	t.Run("sibling namespaced distros without plain prefix are accepted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "siblings.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - customer/acme\n  - customer/beta\n"), 0o600))

		c, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, []string{"customer/acme", "customer/beta"}, c.Distros)
	})

	t.Run("suite distro not in distros list is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mismatch.yaml")
		require.NoError(t, os.WriteFile(path,
			[]byte("signing_key: KEY\ndistros:\n  - debian\nsuites:\n  ubuntu/jammy:\n    origin: x\n"), 0o600))

		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ubuntu/jammy")
		assert.Contains(t, err.Error(), "ubuntu")
	})
}

func TestValidateDistroName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"plain lowercase", "debian", false},
		{"with hyphen", "ubuntu-22", false},
		{"alphanumeric", "epel8", false},
		{"single char", "a", false},
		{"uppercase allowed", "Debian", false},
		{"namespaced", "customer/acme", false},
		{"namespaced both with hyphen", "customer-1/acme-2", false},
		{"leading hyphen", "-debian", true},
		{"empty", "", true},
		{"contains space", "de bian", true},
		{"too deep", "customer/acme/extra", true},
		{"leading slash", "/acme", true},
		{"trailing slash", "customer/", true},
		{"empty middle", "customer//acme", true},
		{"dot segment right", "customer/.", true},
		{"dotdot segment right", "customer/..", true},
		{"leading dash on right", "customer/-acme", true},
		{"leading dash on left", "-customer/acme", true},
		{"plain dot", ".", true},
		{"plain dotdot", "..", true},
		{"non-ascii letter", "débian", true},
		{"control char", "deb\x00ian", true},
		{"underscore", "deb_ian", true},
		{"newline", "deb\nian", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDistroName(tt.input)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSplitSuiteKey(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantDistro string
		wantSuite  string
		wantOK     bool
	}{
		{"plain", "debian/bookworm", "debian", "bookworm", true},
		{"namespaced", "customer/acme/stable", "customer/acme", "stable", true},
		{"no slash", "debian", "", "", false},
		{"empty", "", "", "", false},
		{"leading slash", "/bookworm", "", "", false},
		{"trailing slash", "debian/", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, s, ok := splitSuiteKey(tt.input)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantDistro, d)
			assert.Equal(t, tt.wantSuite, s)
		})
	}
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
