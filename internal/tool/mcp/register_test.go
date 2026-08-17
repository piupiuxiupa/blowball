package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/tool"
)

// TestRegisterAll_DescriptionsMarkCriticalConstraints pins the prompt
// convention (bold + UPPERCASE strong-imperative keywords, >=2 per tool
// description — R4 of the tool-description ruleset) for the five per-user
// mcp_* tools, alongside the key contract phrases each description must carry.
// The descriptions' Execute closures are never invoked, so a nil manager is
// safe here.
func TestRegisterAll_DescriptionsMarkCriticalConstraints(t *testing.T) {
	r := tool.NewRegistry()
	require.NoError(t, RegisterAll(r, NewTools(nil)))

	for _, name := range []string{ToolListServers, ToolAddServer, ToolRemoveServer, ToolCall, ToolListTools} {
		spec, ok := r.Get(name)
		require.True(t, ok, "%s should be registered", name)
		assert.GreaterOrEqual(t, countStrongImperatives(spec.Description), 2,
			"%s description should mark >=2 critical constraints with a strong imperative", name)
	}

	// Key contract phrases survive copy edits.
	add, _ := r.Get(ToolAddServer)
	assert.Contains(t, add.Description, ".blowball/mcp/{name}/config.json")
	assert.Contains(t, add.Description, "DO NOT put secrets in `headers`")
	call, _ := r.Get(ToolCall)
	assert.Contains(t, call.Description, "DO NOT guess")
	assert.Contains(t, call.Description, "NEVER part of the input or")
	listTools, _ := r.Get(ToolListTools)
	assert.Contains(t, listTools.Description, "AUTHORITATIVE")
	assert.Contains(t, listTools.Description, "DO NOT guess")
	listServers, _ := r.Get(ToolListServers)
	assert.Contains(t, listServers.Description, "NEVER shown")
}

// countStrongImperatives counts emphasized imperative keywords (MUST / DO NOT /
// MUST NOT / IMPORTANT / REQUIRED / ONLY / SHOULD / NEVER) in a description.
func countStrongImperatives(desc string) int {
	n := 0
	for _, kw := range []string{"MUST NOT", "MUST", "DO NOT", "IMPORTANT", "REQUIRED", "ONLY", "SHOULD", "NEVER"} {
		n += strings.Count(desc, kw)
	}
	return n
}
