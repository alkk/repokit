package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupRepoFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "long equals",
			args: []string{"--repo=/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "long separate",
			args: []string{"--repo", "/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "short equals",
			args: []string{"-r=/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "short separate",
			args: []string{"-r", "/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "short adjacent",
			args: []string{"-r/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "missing flag",
			args: []string{"add", "--distro", "debian"},
			want: "",
		},
		{
			name: "long is last token",
			args: []string{"add", "--repo"},
			want: "",
		},
		{
			name: "short is last token",
			args: []string{"add", "-r"},
			want: "",
		},
		{
			name: "long followed by flag is not consumed",
			args: []string{"--repo", "--quiet", "add"},
			want: "",
		},
		{
			name: "short followed by flag is not consumed",
			args: []string{"-r", "--quiet", "add"},
			want: "",
		},
		{
			name: "continues past positional to find later flag",
			args: []string{"add", "--repo=/tmp/x"},
			want: "/tmp/x",
		},
		{
			name: "double dash terminator",
			args: []string{"--", "--repo=/tmp/x"},
			want: "",
		},
		{
			name: "empty slice",
			args: nil,
			want: "",
		},
		{
			name: "other long flag is skipped",
			args: []string{"--quiet", "--repo=/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "other short flag is skipped",
			args: []string{"-q", "--repo=/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "value of --public is not mistaken for positional",
			args: []string{"--public", "/srv/web", "--repo", "/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "value of -p is not mistaken for positional",
			args: []string{"-p", "/srv/web", "--repo=/tmp/x", "add"},
			want: "/tmp/x",
		},
		{
			name: "--public as trailing token",
			args: []string{"--public"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, lookupRepoFlag(tc.args))
		})
	}
}

func TestCompletionStateDir(t *testing.T) {
	t.Run("flag in os.Args wins over env and default", func(t *testing.T) {
		flagDir := t.TempDir()
		envDir := t.TempDir()
		t.Setenv(envRepo, envDir)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit", "--repo=" + flagDir, "add", "--distro", ""})

		assert.Equal(t, flagDir, completionStateDir())
	})

	t.Run("env wins over default", func(t *testing.T) {
		envDir := t.TempDir()
		t.Setenv(envRepo, envDir)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit", "add", "--distro", ""})

		assert.Equal(t, envDir, completionStateDir())
	})

	t.Run("default applies when no flag or env", func(t *testing.T) {
		def := t.TempDir()
		t.Setenv(envRepo, "")
		withDefaultStateDir(t, def)
		withOSArgs(t, []string{"repokit", "add", "--distro", ""})

		assert.Equal(t, def, completionStateDir())
	})

	t.Run("empty when default fails", func(t *testing.T) {
		t.Setenv(envRepo, "")
		prev := defaultStateDir
		defaultStateDir = func() (string, error) { return "", assert.AnError }
		t.Cleanup(func() { defaultStateDir = prev })
		withOSArgs(t, []string{"repokit", "add"})

		assert.Empty(t, completionStateDir())
	})
}

func TestDistroNameComplete(t *testing.T) {
	writeConfig := func(t *testing.T, root, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(root, configFilename), []byte(body), 0o600))
	}

	t.Run("empty match returns all configured distros", func(t *testing.T) {
		root := t.TempDir()
		writeConfig(t, root, "signing_key: \"ABC123\"\ndistros:\n  - debian\n  - ubuntu\n")
		t.Setenv(envRepo, root)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		got := d.Complete("")
		require.Len(t, got, 2)
		assert.Equal(t, "debian", got[0].Item)
		assert.Equal(t, "ubuntu", got[1].Item)
	})

	t.Run("prefix match filters", func(t *testing.T) {
		root := t.TempDir()
		writeConfig(t, root, "signing_key: \"ABC123\"\ndistros:\n  - debian\n  - ubuntu\n  - debian-rc\n")
		t.Setenv(envRepo, root)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		got := d.Complete("deb")
		require.Len(t, got, 2)
		assert.Equal(t, "debian", got[0].Item)
		assert.Equal(t, "debian-rc", got[1].Item)
	})

	t.Run("namespaced distro round-trips", func(t *testing.T) {
		root := t.TempDir()
		writeConfig(t, root, "signing_key: \"ABC123\"\ndistros:\n  - debian\n  - customer/acme\n")
		t.Setenv(envRepo, root)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		got := d.Complete("customer/")
		require.Len(t, got, 1)
		assert.Equal(t, "customer/acme", got[0].Item)
	})

	t.Run("missing config returns nil", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(envRepo, root)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		assert.Nil(t, d.Complete(""))
	})

	t.Run("missing state dir returns nil", func(t *testing.T) {
		t.Setenv(envRepo, "")
		prev := defaultStateDir
		defaultStateDir = func() (string, error) { return "", assert.AnError }
		t.Cleanup(func() { defaultStateDir = prev })
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		assert.Nil(t, d.Complete(""))
	})

	t.Run("malformed config returns nil", func(t *testing.T) {
		root := t.TempDir()
		writeConfig(t, root, "signing_key: ok\nsuites: [oops")
		t.Setenv(envRepo, root)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit"})

		var d DistroName
		assert.Nil(t, d.Complete(""))
	})

	t.Run("flag in os.Args wins over env", func(t *testing.T) {
		flagRoot := t.TempDir()
		envRoot := t.TempDir()
		writeConfig(t, flagRoot, "signing_key: \"ABC\"\ndistros:\n  - debian\n")
		writeConfig(t, envRoot, "signing_key: \"ABC\"\ndistros:\n  - ubuntu\n")
		t.Setenv(envRepo, envRoot)
		withDefaultStateDir(t, t.TempDir())
		withOSArgs(t, []string{"repokit", "--repo=" + flagRoot, "add", "--distro", ""})

		var d DistroName
		got := d.Complete("")
		require.Len(t, got, 1)
		assert.Equal(t, "debian", got[0].Item)
	})
}

// withOSArgs swaps os.Args for the duration of a test so completion helpers
// can be driven against a known argv without invoking the parser.
func withOSArgs(t *testing.T, args []string) {
	t.Helper()
	prev := os.Args
	os.Args = args
	t.Cleanup(func() { os.Args = prev })
}
