package main

import "os"

// revision is replaced at build time via -ldflags "-X main.revision=...".
var revision = "unknown"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, revision))
}
