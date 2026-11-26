package main

import (
	"os"

	"github.com/ra1ncandy/soma/internal/cli"
	"github.com/ra1ncandy/soma/internal/sandbox"
)

func main() {
	// Inside a networked sandbox, soma itself is the egress forwarder (see sandbox/egress.go).
	if len(os.Args) > 1 && os.Args[1] == sandbox.ForwarderCommand {
		os.Exit(sandbox.RunNetForwarder(os.Args[2:]))
	}
	if exe, err := os.Executable(); err == nil {
		sandbox.ForwarderBinary = exe
	}
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
