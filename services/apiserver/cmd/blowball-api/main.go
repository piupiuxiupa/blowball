// Command blowball-api is the standalone api-role service binary: the CRUD/
// history/scheduled-task-management partition on server.port plus the
// SchedulerControlService on the internal gRPC listener. It is role-pinned —
// there is no --role flag (passing one exits non-zero via flag parsing) — and
// it also carries the seed subcommand (user management is an api concern).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	apiserver "github.com/lush/blowball/services/apiserver"
	apiserverrun "github.com/lush/blowball/services/apiserver/internal/run"
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
		err = apiserver.RunSeed("blowball-api", os.Args[2:])
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

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath, dataRoot := sharedFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return apiserverrun.Serve(*configPath, *dataRoot)
}

func usage() {
	fmt.Fprint(os.Stderr, `blowball-api is the standalone api-role service of the blowball backend.

Usage:
  blowball-api serve [flags]   Run the api-role HTTP server
  blowball-api seed [flags]    Create a user with a bcrypt-hashed password

Shared flags:
  -f, --config    path to config.yaml (default "config.yaml")
  -d, --data-dir  runtime data root holding data/, logs/, skills/ (default ".")

The api partition serves CRUD/history/scheduled-task-management routes on
server.port. There is no --role flag: the role is pinned to api.

Run "blowball-api <subcommand> -h" for subcommand-specific flags.
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
