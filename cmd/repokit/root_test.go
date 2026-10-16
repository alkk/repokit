package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alkk/repokit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRepoRoot(t *testing.T) {
	t.Run("flag wins over env and default", func(t *testing.T) {
		flagDir := t.TempDir()
		envDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(flagDir, dbFilename), nil, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(envDir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, envDir)
		withDefaultStateDir(t, t.TempDir())

		root, err := resolveRepoRoot(flagDir, "list")
		require.NoError(t, err)
		assert.Equal(t, flagDir, root)
	})

	t.Run("env wins over default", func(t *testing.T) {
		envDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(envDir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, envDir)
		withDefaultStateDir(t, t.TempDir())

		root, err := resolveRepoRoot("", "list")
		require.NoError(t, err)
		assert.Equal(t, envDir, root)
	})

	t.Run("default applies for non-init when populated", func(t *testing.T) {
		defaultDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(defaultDir, dbFilename), nil, 0o600))
		t.Setenv(envRepo, "")
		withDefaultStateDir(t, defaultDir)

		root, err := resolveRepoRoot("", "add")
		require.NoError(t, err)
		// On macOS the temp dir is symlinked through /private; compare resolved paths.
		got, _ := filepath.EvalSymlinks(root)
		want, _ := filepath.EvalSymlinks(defaultDir)
		assert.Equal(t, want, got)
	})

	t.Run("default applies for non-init when empty returns errRepoNotFound", func(t *testing.T) {
		defaultDir := t.TempDir()
		t.Setenv(envRepo, "")
		withDefaultStateDir(t, defaultDir)

		_, err := resolveRepoRoot("", "add")
		require.Error(t, err)
		assert.ErrorIs(t, err, errRepoNotFound)
	})

	t.Run("default applies for init even when empty", func(t *testing.T) {
		defaultDir := t.TempDir()
		t.Setenv(envRepo, "")
		withDefaultStateDir(t, defaultDir)

		root, err := resolveRepoRoot("", "init")
		require.NoError(t, err)
		assert.Equal(t, defaultDir, root)
	})

	t.Run("cwd is not consulted", func(t *testing.T) {
		// place repo.db in cwd to prove resolution does not fall back to it.
		cwd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cwd, dbFilename), nil, 0o600))
		emptyDefault := t.TempDir()
		t.Setenv(envRepo, "")
		t.Chdir(cwd)
		withDefaultStateDir(t, emptyDefault)

		_, err := resolveRepoRoot("", "list")
		require.Error(t, err, "cwd should be ignored even when it contains repo.db")
		assert.ErrorIs(t, err, errRepoNotFound)
	})

	t.Run("init does not require repo.db", func(t *testing.T) {
		empty := t.TempDir()

		root, err := resolveRepoRoot(empty, "init")
		require.NoError(t, err)
		assert.Equal(t, empty, root)
	})

	t.Run("missing repo.db returns errRepoNotFound", func(t *testing.T) {
		empty := t.TempDir()

		_, err := resolveRepoRoot(empty, "list")
		require.Error(t, err)
		assert.ErrorIs(t, err, errRepoNotFound)
	})
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitOK, code)
	assert.Contains(t, stdout.String(), "Usage:")
}

// TestRunCompletionHandler asserts that GO_FLAGS_COMPLETION=1 routes
// completion items through the stdout writer rather than os.Exit(0)-ing the
// process. The "i" prefix exercises subcommand-name completion: go-flags
// should match init and info (alphabetical via sort.Strings on the parser's
// command list).
func TestRunCompletionHandler(t *testing.T) {
	t.Setenv("GO_FLAGS_COMPLETION", "1")
	var stdout, stderr bytes.Buffer
	code := run([]string{"i"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Empty(t, stderr.String(), "stderr must stay clean during completion")
	out := stdout.String()
	assert.Contains(t, out, "init")
	assert.Contains(t, out, "info")
}

// TestRunWithoutCompletionEnvIsUnchanged guards that the handler hook
// introduced for completion does not perturb normal dispatch when
// GO_FLAGS_COMPLETION is unset.
func TestRunWithoutCompletionEnvIsUnchanged(t *testing.T) {
	t.Setenv("GO_FLAGS_COMPLETION", "")
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

func TestRunVersionFlag(t *testing.T) {
	for _, flag := range []string{"--version", "-v"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, &stdout, &stderr, "test-rev")
			assert.Equal(t, exitOK, code, "stderr: %s", stderr.String())
			assert.Equal(t, "test-rev\n", stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--definitely-not-a-flag"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitUsage, code)
	assert.NotEmpty(t, stderr.String())
}

func TestRunInitWithoutDB(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.NotContains(t, stderr.String(), "no repo.db")
}

func TestRunMissingRepoDB(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", dir, "list"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "no repo.db")
}

func TestRunNoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitUsage, code)
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

// TestValidateDistroScope mirrors TestValidateScopeName but exercises the
// namespaced-distro alphabet: a single plain segment or two plain segments
// joined by exactly one '/'. Suite/component/arch keep their slash-rejecting
// validator (see TestValidateScopeName) — this test is only about --distro.
func TestValidateDistroScope(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"plain", "debian", false},
		{"namespaced", "customer/acme", false},
		{"dotted namespace", "7.x/debian", false},
		{"deeper rejected", "customer/acme/extra", true},
		{"leading slash rejected", "/acme", true},
		{"trailing slash rejected", "customer/", true},
		{"empty middle rejected", "customer//acme", true},
		{"leading dash rejected", "-customer/acme", true},
		{"leading dash on right rejected", "customer/-acme", true},
		{"dot left rejected", "./acme", true},
		{"dot right rejected", "customer/.", true},
		{"dotdot right rejected", "customer/..", true},
		{"control char rejected", "customer/acme\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDistroScope(tc.val)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--distro",
					"error message should quote the --distro flag name")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestRequireDistroScope asserts the empty-string rejection and that
// non-empty values delegate to validateDistroScope.
func TestRequireDistroScope(t *testing.T) {
	t.Run("empty rejected", func(t *testing.T) {
		err := requireDistroScope("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--distro")
		assert.Contains(t, err.Error(), "must not be empty")
	})
	t.Run("plain accepted", func(t *testing.T) {
		assert.NoError(t, requireDistroScope("debian"))
	})
	t.Run("namespaced accepted", func(t *testing.T) {
		assert.NoError(t, requireDistroScope("customer/acme"))
	})
	t.Run("invalid rejected", func(t *testing.T) {
		err := requireDistroScope("customer/acme/extra")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--distro")
	})
}

// TestValidateScopeNameStillRejectsSlash guards that the suite/component/arch
// validator did NOT inherit the namespaced acceptance from validateDistroScope.
// A `/` in any of those fields must still be rejected — they are single path
// segments under dists/, never a parent namespace.
func TestValidateScopeNameStillRejectsSlash(t *testing.T) {
	for _, field := range []string{"suite", "component", "arch"} {
		t.Run(field, func(t *testing.T) {
			err := validateScopeName(field, "a/b")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "/")
		})
	}
}

func TestRequireConfiguredDistro(t *testing.T) {
	configured := &config.Config{Distros: []string{"debian", "ubuntu"}}

	cases := []struct {
		name        string
		cfg         *config.Config
		distro      string
		wantErr     bool
		wantContain []string
	}{
		{
			name:        "empty distro rejected",
			cfg:         configured,
			distro:      "",
			wantErr:     true,
			wantContain: []string{"--distro", "must not be empty"},
		},
		{
			name:    "configured distro accepted",
			cfg:     configured,
			distro:  "debian",
			wantErr: false,
		},
		{
			name:    "second configured distro accepted",
			cfg:     configured,
			distro:  "ubuntu",
			wantErr: false,
		},
		{
			name:        "unconfigured distro rejected names value and list",
			cfg:         configured,
			distro:      "raspbian",
			wantErr:     true,
			wantContain: []string{`"raspbian"`, "config.distros", "debian", "ubuntu"},
		},
		{
			name:        "empty distros list rejects any value",
			cfg:         &config.Config{Distros: nil},
			distro:      "debian",
			wantErr:     true,
			wantContain: []string{`"debian"`, "config.distros"},
		},
		{
			name:        "nil config rejects any value without panicking",
			cfg:         nil,
			distro:      "debian",
			wantErr:     true,
			wantContain: []string{`"debian"`, "config.distros"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireConfiguredDistro(tc.cfg, tc.distro)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, s := range tc.wantContain {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

// TestDistroPathTraversalRejected guards every command that lets --distro
// flow into a filesystem path. If a future change forgot the validateScopeName
// gate, a malicious value like "../etc" would let publish or check operate
// on a directory outside the repo root. The test asserts non-zero exit and
// that no per-distro tree was created at the traversal target.
func TestDistroPathTraversalRejected(t *testing.T) {
	traversals := []struct {
		name   string
		distro string
	}{
		{"dot-dot", ".."},
		{"slash-injected", "../etc"},
		{"backslash-injected", "..\\etc"},
		{"leading-dot", ".hidden"},
		{"newline-injected", "debian\nLabel: Forged"},
	}

	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")

	commands := []struct {
		name string
		args func(distro string) []string
	}{
		{
			name: "add",
			args: func(d string) []string {
				return []string{"add", "--distro", d, "--suite", "bookworm", deb}
			},
		},
		{
			name: "publish",
			args: func(d string) []string {
				return []string{"publish", "--distro", d, "--suite", "bookworm", "--no-sign"}
			},
		},
		{
			name: "check",
			args: func(d string) []string {
				return []string{"check", "--distro", d}
			},
		},
	}

	for _, c := range commands {
		for _, tc := range traversals {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				public := t.TempDir()
				initRepoForTest(t, root)
				before := snapshotRoot(t, root)
				beforePublic := snapshotRoot(t, public)

				args := append([]string{"--repo", root, "--public", public}, c.args(tc.distro)...)
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr, "test-rev")

				require.NotEqualf(t, exitOK, code, "expected non-zero exit; stderr=%s", stderr.String())
				assert.Contains(t, stderr.String(), "--distro", "error should name the rejected flag")

				assert.Equal(t, before, snapshotRoot(t, root), "no state-dir mutation expected on a rejected --distro")
				assert.Equal(t, beforePublic, snapshotRoot(t, public), "no public-dir mutation expected on a rejected --distro")
			})
		}
	}
}

// snapshotRoot returns the sorted set of repo-root-relative paths under root.
// Used by the traversal regression test to assert that a rejected --distro
// run leaves the on-disk tree unchanged.
func snapshotRoot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return fmt.Errorf("computing relative path: %w", rerr)
		}
		out = append(out, rel)
		return nil
	})
	require.NoError(t, err)
	return out
}

func TestNeedsPublic(t *testing.T) {
	cases := map[string]bool{
		"add":        true,
		"delete":     false,
		"publish":    true,
		"check":      true,
		"init":       false,
		"info":       false,
		"list":       false,
		"search":     false,
		"completion": false,
		"":           false,
		"unknown":    false,
	}
	for cmd, want := range cases {
		t.Run(cmd, func(t *testing.T) {
			assert.Equal(t, want, needsPublic(cmd))
		})
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("missing file returns empty config", func(t *testing.T) {
		root := t.TempDir()
		cfg, err := loadConfig(root)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Empty(t, cfg.SigningKey)
		assert.Empty(t, cfg.PublicDir)
		assert.NotNil(t, cfg.Suites)
	})

	t.Run("parses populated config", func(t *testing.T) {
		root := t.TempDir()
		body := "signing_key: \"ABC123\"\npublic_dir: \"/srv/public\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(root, configFilename), []byte(body), 0o600))
		cfg, err := loadConfig(root)
		require.NoError(t, err)
		assert.Equal(t, "ABC123", cfg.SigningKey)
		assert.Equal(t, "/srv/public", cfg.PublicDir)
	})

	t.Run("malformed yaml returns wrapped error", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, configFilename),
			[]byte("signing_key: ok\nsuites: [oops"), 0o600))
		_, err := loadConfig(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "loading config")
	})
}

func TestResolvePublicDir(t *testing.T) {
	t.Run("flag wins over config", func(t *testing.T) {
		root := t.TempDir()
		flagDir := t.TempDir()
		cfgDir := t.TempDir()
		cfg := &config.Config{PublicDir: cfgDir}
		got, err := resolvePublicDir(flagDir, cfg, root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(flagDir), got)
	})

	t.Run("config used when flag empty", func(t *testing.T) {
		root := t.TempDir()
		cfgDir := t.TempDir()
		cfg := &config.Config{PublicDir: cfgDir}
		got, err := resolvePublicDir("", cfg, root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(cfgDir), got)
	})

	t.Run("flag REPLACE_ME falls through to config", func(t *testing.T) {
		root := t.TempDir()
		cfgDir := t.TempDir()
		cfg := &config.Config{PublicDir: cfgDir}
		got, err := resolvePublicDir(placeholderValue, cfg, root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(cfgDir), got)
	})

	t.Run("both unset errors with friendly message", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("", &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir is not set")
		assert.Contains(t, err.Error(), envPublic)
		assert.Contains(t, err.Error(), filepath.Join(root, configFilename))
	})

	t.Run("config REPLACE_ME treated as missing", func(t *testing.T) {
		root := t.TempDir()
		cfg := &config.Config{PublicDir: placeholderValue}
		_, err := resolvePublicDir("", cfg, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir is not set")
	})

	t.Run("nil config treated as missing", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("", nil, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "public_dir is not set")
	})

	t.Run("relative path rejected", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("relative/path", &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an absolute path")
	})

	t.Run("missing directory rejected", func(t *testing.T) {
		root := t.TempDir()
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := resolvePublicDir(missing, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "directory does not exist")
		assert.Contains(t, err.Error(), "create it manually")
	})

	t.Run("non-directory rejected", func(t *testing.T) {
		root := t.TempDir()
		filePath := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(filePath, nil, 0o600))
		_, err := resolvePublicDir(filePath, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})

	t.Run("control char rejected", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("/tmp/foo\nLabel: x", &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "control")
	})

	t.Run("nul byte rejected", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("/tmp/foo\x00bar", &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "control")
	})

	t.Run("equal to root rejected", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir(root, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not equal or be nested")
	})

	t.Run("nested inside root rejected", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "public")
		require.NoError(t, os.MkdirAll(nested, 0o755))
		_, err := resolvePublicDir(nested, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not equal or be nested")
	})

	t.Run("root nested inside public rejected", func(t *testing.T) {
		public := t.TempDir()
		root := filepath.Join(public, "state")
		require.NoError(t, os.MkdirAll(root, 0o755))
		_, err := resolvePublicDir(public, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not equal or be nested")
	})

	t.Run("sibling directories accepted", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "state")
		public := filepath.Join(parent, "public")
		require.NoError(t, os.MkdirAll(root, 0o755))
		require.NoError(t, os.MkdirAll(public, 0o755))
		got, err := resolvePublicDir(public, &config.Config{}, root)
		require.NoError(t, err)
		assert.Equal(t, public, got)
	})

	t.Run("carriage return rejected", func(t *testing.T) {
		root := t.TempDir()
		_, err := resolvePublicDir("/tmp/foo\rEvil", &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "control")
	})

	// A symlinked public dir whose target lives inside the state dir would
	// expose repo.db / config.yaml under the webserver docroot — the exact
	// failure mode the split was built to prevent. Lexical Rel comparison
	// alone is not enough; resolvePublicDir must follow symlinks.
	t.Run("symlink into state dir rejected", func(t *testing.T) {
		root := t.TempDir()
		realPublic := filepath.Join(root, "inside")
		require.NoError(t, os.MkdirAll(realPublic, 0o755))
		linkParent := t.TempDir()
		link := filepath.Join(linkParent, "public")
		require.NoError(t, os.Symlink(realPublic, link))
		_, err := resolvePublicDir(link, &config.Config{}, root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not equal or be nested")
	})
}

func TestRunPopulatesPublicDirForNeedyCommand(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "--public", public, "check"},
		&stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

func TestRunUsesEnvPublic(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	t.Setenv(envPublic, public)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "check"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

func TestRunReadsPublicDirFromConfig(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	body := "signing_key: \"ABC123\"\npublic_dir: \"" + public + "\"\ndistros:\n  - debian\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, configFilename), []byte(body), 0o600))
	t.Setenv(envPublic, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "check"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

func TestRunErrorsWhenPublicMissingForNeedyCommand(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	t.Setenv(envPublic, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "check"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "public_dir is not set")
}

// TestRunCheckToleratesMissingSigningKey guards against the regression where
// loadConfig (called for every needsPublic command) bubbled up an
// ErrMissingSigningKey from config.Load — breaking add/delete/check whenever
// signing_key was blank, even though those commands never sign.
func TestRunCheckToleratesMissingSigningKey(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	body := "signing_key: \"\"\npublic_dir: \"" + public + "\"\ndistros:\n  - debian\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, configFilename), []byte(body), 0o600))
	t.Setenv(envPublic, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "check"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

func TestRunSkipsPublicResolutionForListAndInit(t *testing.T) {
	root := t.TempDir()
	initRepoForTest(t, root)
	t.Setenv(envPublic, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "list"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitOK, code, "list should not require --public; stderr=%s", stderr.String())

	stdout.Reset()
	stderr.Reset()
	freshRoot := t.TempDir()
	code = run([]string{"--repo", freshRoot, "init"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, exitOK, code, "init should not require --public; stderr=%s", stderr.String())
}

// TestRunCheckWithReplaceMeFallsBackToConfig guards the corner case where
// --public / $REPOKIT_PUBLIC is set to the literal placeholder value. The
// flag-load skip optimization must still load config.yaml in that case so
// resolvePublicDir's REPLACE_ME-as-unset rule can fall back to public_dir
// from config.
func TestRunCheckWithReplaceMeFallsBackToConfig(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	body := "signing_key: \"ABC123\"\npublic_dir: \"" + public + "\"\ndistros:\n  - debian\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, configFilename), []byte(body), 0o600))
	t.Setenv(envPublic, placeholderValue)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "check"}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
}

// TestRunMalformedConfigBlocksAllStateCommands asserts that any command that
// reads the state dir surfaces a malformed config.yaml at startup, even when
// --public is supplied and the command itself never reads cfg. Loading and
// validating the config unconditionally is the whole point — a broken config
// must not be ignored just because you happened to pass --public.
func TestRunMalformedConfigBlocksAllStateCommands(t *testing.T) {
	root := t.TempDir()
	public := t.TempDir()
	initRepoForTest(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, configFilename),
		[]byte("signing_key: ok\nsuites: [oops"), 0o600))
	t.Setenv(envPublic, "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--repo", root, "--public", public, "check"},
		&stdout, &stderr, "test-rev")
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "loading config")
}

// TestRunInfoWithoutHomeDir guards against info regressing to require a
// resolvable home directory. info inspects a .deb file and never reads the
// state dir, so a defaultStateDir failure (no $HOME in minimal containers)
// must not block it.
func TestRunInfoWithoutHomeDir(t *testing.T) {
	deb := fixtureDeb(t, "hello-gz_1.0_amd64.deb")
	t.Setenv(envRepo, "")
	prev := defaultStateDir
	defaultStateDir = func() (string, error) {
		return "", errors.New("simulated missing home")
	}
	t.Cleanup(func() { defaultStateDir = prev })

	var stdout, stderr bytes.Buffer
	code := run([]string{"info", deb}, &stdout, &stderr, "test-rev")
	require.Equal(t, exitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "Package:")
}
