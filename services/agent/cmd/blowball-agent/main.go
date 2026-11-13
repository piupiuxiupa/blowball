// Command blowball-agent is the standalone agent-role service binary: the
// streaming message endpoint, turn lifecycle, MCP tool list, and the
// ScheduledRuntimeService on the internal gRPC listener, all on
// server.agent_port. It is role-pinned — there is no --role flag (passing one
// exits non-zero via flag parsing).
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	agentrun "github.com/lush/blowball/services/agent/internal/run"
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
	return agentrun.Serve(*configPath, *dataRoot)
}

func usage() {
	fmt.Fprint(os.Stderr, `blowball-agent is the standalone agent-role service of the blowball backend.

Usage:
  blowball-agent serve [flags]   Run the agent-role HTTP server

Shared flags:
  -f, --config    path to config.yaml (default "config.yaml")
  -d, --data-dir  runtime data root holding data/, logs/, skills/ (default ".")

The agent partition serves the streaming message endpoint, turn lifecycle,
and MCP tool list on server.agent_port, and accepts scheduled executions
over the internal gRPC link. There is no --role flag: the role is pinned
to agent.

Run "blowball-agent serve -h" for flag details.
`)
}

// sharedFlags registers the -f/--config and -d/--data-dir flags.
func sharedFlags(fs *flag.FlagSet) (configPath, dataDir *string) {
	configPath = fs.String("config", "config.yaml", "path to config.yaml")
	fs.StringVar(configPath, "f", "config.yaml", "shorthand for --config")
	dataDir = fs.String("data-dir", ".", "runtime data root (holds data/, logs/, skills/)")
	fs.StringVar(dataDir, "d", ".", "shorthand for --data-dir")
	return configPath, dataDir
}
