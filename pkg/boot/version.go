package boot

import "fmt"

// Build-metadata vars, injected at build time via
//
//	-ldflags "-X github.com/lush/blowball/pkg/boot.Version=... ..."
//
// by every service Dockerfile (deploy/*/Dockerfile) and the Jenkins pipeline.
// The defaults keep `go build`/`go run` working with no flags; the mains
// surface them through the `version` subcommand. The linker silently ignores
// -X for packages that do not declare the var, so one Jenkins arg set serves
// all binaries.
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
