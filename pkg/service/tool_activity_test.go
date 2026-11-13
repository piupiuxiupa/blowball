package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
)

func TestExtractToolActivity_LegacySectionRoundTrip(t *testing.T) {
	content := `## Primary Request and Intent
continue safely

## Tool Activity
- grep: 2 calls, 1 succeeded, 1 failed
  - args={"query":"foo"} outcome=one
  - args={"query":"bar"} outcome=error`

	body, activity := ExtractToolActivity(content)

	assert.NotContains(t, body, "## Tool Activity")
	require.Len(t, activity, 1)
	assert.Equal(t, "grep", activity[0].ToolName)
	assert.Equal(t, 2, activity[0].Calls)
	assert.Equal(t, 1, activity[0].Success)
	assert.Equal(t, 1, activity[0].Errors)
	require.Len(t, activity[0].Examples, 2)

	roundTrip := DecodeToolActivity(MarshalToolActivity(activity))
	assert.Equal(t, activity, roundTrip)
}

func TestMergeToolActivity_RecompactionIsAdditiveAndBounded(t *testing.T) {
	prior := []ToolActivity{{
		ToolName: "grep",
		Calls:    2,
		Success:  1,
		Errors:   1,
		Examples: []string{"old-1", "old-2"},
	}}
	current := []ToolActivity{
		{
			ToolName: "grep",
			Calls:    1,
			Success:  1,
			Examples: []string{"old-2", "new-1", "new-2"},
		},
		{ToolName: "bash", Calls: 1, Success: 1, Examples: []string{"bash-1"}},
	}

	merged := MergeToolActivity(prior, current)

	require.Len(t, merged, 2)
	assert.Equal(t, "grep", merged[0].ToolName)
	assert.Equal(t, 3, merged[0].Calls)
	assert.Equal(t, 2, merged[0].Success)
	assert.Equal(t, 1, merged[0].Errors)
	assert.Equal(t, []string{"old-1", "old-2", "new-1"}, merged[0].Examples)
	assert.Equal(t, 4, TotalToolActivityCalls(merged))
	assert.Equal(t, 1, strings.Count(RenderToolActivityLedger(merged), "- grep:"))
}

func TestBuildToolActivity_BreaksDownMCPCallByServerAndTool(t *testing.T) {
	calls := []agent.ToolCall{
		{ID: "c1", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"gildata","tool":"search","args":{}}`}},
		{ID: "c2", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"gildata","tool":"search","args":{}}`}},
		{ID: "c3", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"gildata","tool":"quote","args":{}}`}},
		{ID: "c4", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"wiki","tool":"search","args":{}}`}},
		{ID: "c5", Function: agent.ToolCallFunction{Name: "mcp_list_tools", Arguments: `{"server":"gildata"}`}},
	}
	results := map[string]string{
		"c1": `{"status":0,"result":"ok"}`,
		"c2": `{"status":1,"error":"bad"}`,
		"c3": `{"status":0,"result":"ok"}`,
		"c4": `{"status":0,"result":"ok"}`,
		"c5": `{"status":0,"result":[]}`,
	}

	ledger := BuildToolActivity(calls, results)
	require.Len(t, ledger, 2)
	mcp := ledger[0]
	assert.Equal(t, "mcp_call", mcp.ToolName)
	assert.Equal(t, 4, mcp.Calls)
	assert.Equal(t, 3, mcp.Success)
	assert.Equal(t, 1, mcp.Errors)
	assert.Equal(t, 5, TotalToolActivityCalls(ledger), "child rows must not be counted twice")
	assert.Empty(t, ledger[1].MCPServers, "mcp_list_tools is not a remote invocation")

	require.Len(t, mcp.MCPServers, 2)
	gildata := mcp.MCPServers[0]
	assert.Equal(t, "gildata", gildata.ServerName)
	require.Len(t, gildata.Tools, 2)
	assert.Equal(t, MCPToolActivity{ToolName: "search", Calls: 2, Success: 1, Errors: 1}, gildata.Tools[0])
	assert.Equal(t, MCPToolActivity{ToolName: "quote", Calls: 1, Success: 1}, gildata.Tools[1])
	wiki := mcp.MCPServers[1]
	assert.Equal(t, "wiki", wiki.ServerName)
	assert.Equal(t, []MCPToolActivity{{ToolName: "search", Calls: 1, Success: 1}}, wiki.Tools)
}

func TestMergeToolActivity_MergesNestedMCPActivityAndLegacyResidual(t *testing.T) {
	prior := []ToolActivity{{
		ToolName: "mcp_call", Calls: 3, Success: 2, Errors: 1,
		MCPServers: []MCPServerActivity{{
			ServerName: "gildata",
			Tools:      []MCPToolActivity{{ToolName: "search", Calls: 2, Success: 1, Errors: 1}},
		}},
	}}
	current := BuildToolActivity([]agent.ToolCall{
		{ID: "n1", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"gildata","tool":"search"}`}},
		{ID: "n2", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"wiki","tool":"search"}`}},
	}, map[string]string{
		"n1": `{"status":0,"result":"ok"}`,
		"n2": `{"status":0,"result":"ok"}`,
	})

	merged := MergeToolActivity(prior, current)
	require.Len(t, merged, 1)
	mcp := merged[0]
	assert.Equal(t, 5, mcp.Calls)
	assert.Equal(t, 4, mcp.Success)
	assert.Equal(t, 1, mcp.Errors)
	assert.Equal(t, 5, TotalToolActivityCalls(merged))
	require.Len(t, mcp.MCPServers, 2)
	assert.Equal(t, 3, mcp.MCPServers[0].Tools[0].Calls)
	assert.Equal(t, 1, mcp.MCPServers[1].Tools[0].Calls)
	rendered := RenderToolActivityLedger(merged)
	assert.Contains(t, rendered, "  - gildata")
	assert.Contains(t, rendered, "    - search: 3 calls, 2 succeeded, 1 failed")
	assert.Contains(t, rendered, "  - wiki")
	assert.Contains(t, rendered, "  - unattributed: 1 calls, 1 succeeded, 0 failed")
}

func TestToolActivity_MCPJSONRoundTripAndLegacyResidual(t *testing.T) {
	ledger := BuildToolActivity([]agent.ToolCall{
		{ID: "valid", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `{"server":"gildata","tool":"search"}`}},
		{ID: "invalid", Function: agent.ToolCallFunction{Name: "mcp_call", Arguments: `not-json`}},
	}, map[string]string{
		"valid":   `{"status":0,"result":"ok"}`,
		"invalid": `{"status":1,"error":"bad args"}`,
	})
	require.Len(t, ledger, 1)
	roundTrip := DecodeToolActivity(MarshalToolActivity(ledger))
	assert.Equal(t, ledger, roundTrip)
	assert.Contains(t, RenderToolActivityLedger(roundTrip), "unattributed: 1 calls, 0 succeeded, 1 failed")

	legacy := DecodeToolActivity([]byte(`[{"tool_name":"mcp_call","calls":2,"success":2,"errors":0}]`))
	require.Len(t, legacy, 1)
	assert.Empty(t, legacy[0].MCPServers)
	assert.Contains(t, RenderToolActivityLedger(legacy), "unattributed: 2 calls, 2 succeeded, 0 failed")
}

func TestBuildToolActivity_PreservesEveryMCPPair(t *testing.T) {
	calls := make([]agent.ToolCall, 0, 50)
	results := make(map[string]string, 50)
	for i := range 50 {
		id := fmt.Sprintf("call-%02d", i)
		calls = append(calls, agent.ToolCall{
			ID: id,
			Function: agent.ToolCallFunction{
				Name:      "mcp_call",
				Arguments: fmt.Sprintf(`{"server":"server-%02d","tool":"search"}`, i),
			},
		})
		results[id] = `{"status":0,"result":"ok"}`
	}
	ledger := BuildToolActivity(calls, results)
	require.Len(t, ledger, 1)
	require.Len(t, ledger[0].MCPServers, 50)
	assert.Equal(t, 50, ledger[0].Calls)
	assert.Equal(t, 50, TotalToolActivityCalls(ledger))
	rendered := RenderToolActivityLedger(ledger)
	for i := range 50 {
		assert.Contains(t, rendered, fmt.Sprintf("  - server-%02d\n", i))
	}
	assert.NotContains(t, rendered, "unattributed")
}

func TestExtractToolActivity_PreservesFencedMarkdown(t *testing.T) {
	content := strings.Join([]string{
		"## Critical Context",
		"```markdown",
		"## Tool Activity",
		"## Example",
		"keep this code",
		"```",
		"after fence",
		"",
		"## Tool Activity",
		"- grep: 2 calls, 1 succeeded, 1 failed",
	}, "\n")

	body, activity := ExtractToolActivity(content)

	assert.Contains(t, body, "```markdown")
	assert.Contains(t, body, "## Tool Activity\n## Example")
	assert.Contains(t, body, "keep this code")
	assert.Contains(t, body, "after fence")
	assert.Equal(t, 1, strings.Count(body, "## Tool Activity"),
		"only the fenced example heading should remain in the body")
	require.Len(t, activity, 1)
	assert.Equal(t, 2, activity[0].Calls)
}
