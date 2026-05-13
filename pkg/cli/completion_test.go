package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletionCmdShellSentinels(t *testing.T) {
	cases := []struct {
		shell    string
		sentinel string
	}{
		{"bash", "complete -F _repokit repokit"},
		{"zsh", "compdef _repokit repokit"},
		{"fish", "complete -c repokit"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{"completion", tc.shell}, &stdout, &stderr, "test-rev")
			require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
			assert.Empty(t, stderr.String())
			out := stdout.String()
			assert.NotEmpty(t, out)
			assert.Contains(t, out, tc.sentinel)
		})
	}
}

func TestCompletionCmdUnsupportedShell(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"completion", "powershell"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.Empty(t, stdout.String(), "no partial script may leak to stdout when shell is rejected")
	msg := stderr.String()
	assert.Contains(t, msg, "powershell")
	assert.Contains(t, msg, "bash")
	assert.Contains(t, msg, "zsh")
	assert.Contains(t, msg, "fish")
}

func TestCompletionCmdMissingShell(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"completion"}, &stdout, &stderr, "test-rev")
	assert.Equal(t, ExitUsage, code)
	assert.NotEmpty(t, stderr.String())
	assert.Contains(t, stderr.String(), "shell",
		"error must name the missing positional so operators know what to supply")
}

// TestCompletionCmdSkipsStateDir guards the resolveRepoRoot short-circuit:
// completion must work even when no repo has been initialized and no $HOME is
// resolvable, because the install one-liners run before any repo exists.
func TestCompletionCmdSkipsStateDir(t *testing.T) {
	t.Setenv(envRepo, "")
	prev := defaultStateDir
	defaultStateDir = func() (string, error) { return "", assert.AnError }
	t.Cleanup(func() { defaultStateDir = prev })

	var stdout, stderr bytes.Buffer
	code := Run([]string{"completion", "bash"}, &stdout, &stderr, "test-rev")
	require.Equal(t, ExitOK, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "_repokit")
}

func TestSupportedShells(t *testing.T) {
	got := supportedShells()
	assert.Equal(t, "bash, fish, zsh", got)
}
