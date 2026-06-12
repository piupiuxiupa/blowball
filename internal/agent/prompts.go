package agent

import (
	"context"
	"fmt"
	"runtime"
)

func AppendSystemPromptEnv(ctx context.Context) (env string) {
	workspace := ctx.Value("workspace")
	envrionment := `
	# Environment
	You have been invoked in the following environment:
	- Primary working directory: %s
	- Platform: %s
	- OS Version: %s
	- Assistant knowledge cutoff is August 2025.
	`
	env += fmt.Sprintf(envrionment, workspace, runtime.GOARCH, runtime.GOOS)
	return
}
