// Package buildinfo holds the build-metadata vars injected via
//
//	-ldflags "-X github.com/lush/blowball/pkg/buildinfo.Version=... ..."
//
// by every service Dockerfile (deploy/*/Dockerfile) and the Jenkins pipeline.
// It is a deliberately dependency-free leaf package so every binary —
// including the Scheduler, whose dependency boundary forbids main-service
// packages — can link and report it. The defaults keep `go build`/`go run`
// working with no flags; the linker silently ignores -X for vars a binary
// does not reference, so one Jenkins arg set serves all services.
package buildinfo

import "fmt"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// VersionString renders the build metadata in the one-line form the
// `version` subcommands print.
func VersionString() string {
	return fmt.Sprintf("version %s (commit %s, built %s)", Version, Commit, BuildTime)
}
