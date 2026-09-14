// Package main is the blowball unified CLI entry point.
//
// The binary exposes `serve` and `seed` subcommands; both accept the shared
// flags:
//
//	-f, --config   path to config.yaml (default "config.yaml")
//	-d, --data-dir runtime data root holding data/, logs/, skills/ (default ".")
//
// Running the binary with no subcommand prints help and exits non-zero.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(os.Args[2:])
	case "seed":
		err = seedCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}

	switch {
	case errors.Is(err, flag.ErrHelp):
		// -h/--help on a subcommand: the flag package already printed usage.
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Blowball is the multi-agent chat workspace backend.

Usage:
  blowball serve [flags]   Run the HTTP server
  blowball seed [flags]    Create a user with a bcrypt-hashed password

Shared flags:
  -f, --config    path to config.yaml (default "config.yaml")
  -d, --data-dir  runtime data root holding data/, logs/, skills/ (default ".")

Run "blowball <subcommand> -h" for subcommand-specific flags.
`)
}

// sharedFlags registers the -f/--config and -d/--data-dir flags every
// subcommand accepts. The short and long forms write the same variable.
func sharedFlags(fs *flag.FlagSet) (configPath, dataDir *string) {
	configPath = fs.String("config", "config.yaml", "path to config.yaml")
	fs.StringVar(configPath, "f", "config.yaml", "shorthand for --config")
	dataDir = fs.String("data-dir", ".", "runtime data root (holds data/, logs/, skills/)")
	fs.StringVar(dataDir, "d", ".", "shorthand for --data-dir")
	return configPath, dataDir
}
