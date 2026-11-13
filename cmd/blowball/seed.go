package main

import (
	apiserver "github.com/lush/blowball/services/apiserver"
)

// seedCmd is the `seed` subcommand. The implementation lives in
// services/apiserver (RunSeed) so the blowball-api service binary shares it;
// see the service-roles spec's standalone-binary requirement.
func seedCmd(args []string) error {
	return apiserver.RunSeed("blowball", args)
}
