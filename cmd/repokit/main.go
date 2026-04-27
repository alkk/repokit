package main

import (
	"io"
	"os"

	"github.com/alkk/repokit/pkg/cli"
)

// revision is replaced at build time via -ldflags "-X main.revision=...".
var revision = "unknown"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	return cli.Run(argv, stdout, stderr, revision)
}
