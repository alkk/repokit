package main

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed completion/bash.sh completion/zsh.sh completion/fish.sh
var completionFS embed.FS

// completionScripts holds the shell wrapper script for each supported shell,
// keyed by the shell name accepted as the positional argument. Populated at
// init() so a missing embed surfaces at process start rather than first TAB.
var completionScripts = map[string]string{
	"bash": mustReadCompletion("completion/bash.sh"),
	"zsh":  mustReadCompletion("completion/zsh.sh"),
	"fish": mustReadCompletion("completion/fish.sh"),
}

// mustReadCompletion loads an embedded script, panicking if the file is
// missing. The embed directive guarantees presence at build time, so a panic
// here means the directive and the map drifted apart — caught immediately at
// program start instead of on first user invocation.
func mustReadCompletion(name string) string {
	b, err := completionFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("completion: missing embedded script %s: %v", name, err))
	}
	return string(b)
}

// CompletionCmd emits a shell-specific wiring script that, when sourced, makes
// the shell delegate TAB completion for `repokit` back to the binary itself
// via GO_FLAGS_COMPLETION.
type CompletionCmd struct {
	Args struct {
		Shell string `positional-arg-name:"shell" required:"yes"`
	} `positional-args:"yes"`
}

// Execute runs the completion subcommand.
func (cmd *CompletionCmd) Execute(c *Context) int {
	script, ok := completionScripts[cmd.Args.Shell]
	if !ok {
		fmt.Fprintf(c.Stderr,
			"completion: unsupported shell %q (valid: %s)\n",
			cmd.Args.Shell, supportedShells())
		return exitUsage
	}
	fmt.Fprint(c.Stdout, script)
	return exitOK
}

// supportedShells returns the sorted list of shells with embedded wrappers,
// used to render the error message for unsupported values.
func supportedShells() string {
	names := make([]string, 0, len(completionScripts))
	for name := range completionScripts {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
