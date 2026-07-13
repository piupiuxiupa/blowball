// Package main is the blowball unified CLI entry point.
//
// The cobra root exposes `serve` and `seed` subcommands and two persistent flags
// inherited by both:
//
//	-f, --config   path to config.yaml (default "config.yaml")
//	-d, --data-dir runtime data root holding data/, logs/, skills/ (default ".")
//
// Running the binary with no subcommand prints help and exits non-zero. Each
// subcommand parses the same persistent flags; `serve` runs the HTTP server and `seed`
// creates a user.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the root cobra command with the persistent flags and subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "blowball",
		Short: "Blowball multi-agent chat workspace backend",
		Long: "Blowball is the multi-agent chat workspace backend.\n\n" +
			"Pick a subcommand to continue. Both subcommands accept the shared\n" +
			"-f/--config and -d/--data-dir flags.",
		// A bare invocation (no subcommand) prints help and exits non-zero so the
		// process never silently does nothing. --help is intercepted by cobra first and
		// exits zero. Using Run (not RunE) and os.Exit keeps cobra from echoing an
		// "Error:" line on top of the help.
		Run: func(cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
			os.Exit(1)
		},
		// Disable cobra's auto-generated `completion` command; this is a backend
		// server, not a shell-driven CLI.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}

	root.PersistentFlags().StringP("config", "f", "config.yaml", "path to config.yaml")
	root.PersistentFlags().StringP("data-dir", "d", ".", "runtime data root (holds data/, logs/, skills/)")

	root.AddCommand(newServeCmd())
	root.AddCommand(newSeedCmd())

	return root
}
