package scheduler_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchedulerDependencyBoundary enforces the standalone-service contract:
// the Scheduler may import transport DTOs, pure packages and its own private
// packages, but never the Agent/turn runtime, the main-service store or
// services, LLM/MCP clients, or workspace/executor code.
func TestSchedulerDependencyBoundary(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	cmd := exec.Command(goTool, "list", "-deps", "./...")
	out, err := cmd.Output()
	require.NoError(t, err)
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	module := "github.com/lush/blowball/"
	forbidden := []string{
		"internal/agent",
		"internal/service",
		"internal/turn",
		"internal/run",
		"internal/handler",
		"internal/scheduledruntime",
		"internal/store/mysql",
		"internal/store/redis",
		"internal/store/fs",
		"internal/tool",
		"internal/storage",
		"internal/artifact",
		"internal/memory",
		"internal/prompt",
		"internal/skillmarket",
		"internal/mcpmarket",
		"internal/llmraw",
		"internal/stream",
	}
	for _, dep := range deps {
		if !strings.HasPrefix(dep, module) {
			continue
		}
		rel := strings.TrimPrefix(dep, module)
		for _, bad := range forbidden {
			assert.False(t, rel == bad || strings.HasPrefix(rel, bad+"/"),
				"scheduler must not depend on %s", dep)
		}
	}
	// External LLM/MCP SDKs must not leak into the Scheduler.
	for _, dep := range deps {
		assert.NotContains(t, dep, "github.com/openai/openai-go")
		assert.NotContains(t, dep, "/mcp-go")
	}
}
