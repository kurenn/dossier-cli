// Command dossier is a command-line client for a Dossier vault's API.
//
// This file is deliberately almost empty. Everything it would otherwise do lives in
// internal/cli, where a test can reach it: the exit code a command produces is the part
// of a CLI that scripts and agents actually depend on, and code in package main is the
// hardest code in a Go program to test.
package main

import (
	"os"

	"github.com/kurenn/dossier-cli/internal/cli"
)

// version is overwritten at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
//
// The default says "dev" rather than a plausible-looking number, because a binary built
// from a dirty tree claiming to be 1.2.0 is worse than one admitting it does not know.
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}, version))
}
