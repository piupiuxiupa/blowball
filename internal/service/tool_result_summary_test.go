package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lush/blowball/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeToolResult_structuredIdentityAndBounds(t *testing.T) {
	call := agent.ToolCall{
		ID:       "call-123",
		Function: agent.ToolCallFunction{Name: "executor_bash", Arguments: `{"command":"go test ./internal/agent || echo 'identifier call-123'"}`},
	}
	full := `{"status":1,"error":"exit status 1: identifier call-123 failed"}`
	got := SummarizeToolResult(call, full, 80)
	assert.Contains(t, got, `"tool_name":"executor_bash"`)
	assert.Contains(t, got, `"tool_call_id":"call-123"`)
	assert.Contains(t, got, `"status":"error"`)
	assert.Contains(t, got, "identifier call-123 failed")
	assert.Contains(t, got, `"retrieval_ref":"tool_calls/`)
	assert.LessOrEqual(t, len([]rune(got)), 80*4+1)
}

func TestSummarizeToolResult_unknownToolHeadTailUTF8Safe(t *testing.T) {
	call := agent.ToolCall{ID: "u-1", Function: agent.ToolCallFunction{Name: "unknown_tool", Arguments: `{}`}}
	full := strings.Repeat("界", 2000)
	got := SummarizeToolResult(call, full, 50)
	assert.Contains(t, got, "界")
	assert.True(t, utf8.ValidString(got))
	assert.Less(t, len([]rune(got)), 2000)
}

func TestSummarizeToolResult_shellExitAndTruncation(t *testing.T) {
	call := agent.ToolCall{ID: "b1", Function: agent.ToolCallFunction{Name: "executor_bash", Arguments: `{"command":"cat big.txt"}`}}
	full := `{"status":0,"result":{"output":"head\n` + strings.Repeat("x", 3000) + `\ntail","exit_code":0,"truncated":true}}`
	got := SummarizeToolResult(call, full, 40)
	assert.Contains(t, got, `"exit_code":0`)
	assert.Contains(t, got, `"truncated":true`)
}

func TestRenderToolActivity_countsByTool(t *testing.T) {
	calls := []agent.ToolCall{
		{ID: "a", Function: agent.ToolCallFunction{Name: "grep"}},
		{ID: "b", Function: agent.ToolCallFunction{Name: "grep"}},
		{ID: "c", Function: agent.ToolCallFunction{Name: "bash"}},
	}
	results := map[string]string{
		"a": `{"status":0,"result":{"path":"a.go","total":1}}`,
		"b": `{"status":1,"error":"not found: id-42"}`,
		"c": `{"status":0,"result":{"output":"ok","exit_code":0}}`,
	}
	got := RenderToolActivity(calls, results)
	assert.Contains(t, got, "- grep: 2 calls, 1 succeeded, 1 failed")
	assert.Contains(t, got, "- bash: 1 calls, 1 succeeded, 0 failed")
}

func TestRenderToolActivity_recompactionMergedLedgerIsBounded(t *testing.T) {
	// Re-compaction feeds the prior checkpoint into one fresh summary. The
	// resulting ledger must not retain duplicate per-call examples indefinitely;
	// only the bounded aggregate/examples are rendered.
	var calls []agent.ToolCall
	results := map[string]string{}
	for i := 0; i < 100; i++ {
		id := "call-" + string(rune('A'+i%26))
		calls = append(calls, agent.ToolCall{ID: id, Function: agent.ToolCallFunction{Name: "grep"}})
		results[id] = `{"status":0,"result":{"path":"a.go","total":1}}`
	}
	got := RenderToolActivity(calls, results)
	assert.Contains(t, got, "- grep: 100 calls, 100 succeeded, 0 failed")
	assert.Equal(t, 3, strings.Count(got, "\n"), "one header line plus at most three bounded examples")
}

func TestSummarizeToolResult_neverReturnsEmptyForNilFull(t *testing.T) {
	got := SummarizeToolResult(agent.ToolCall{ID: "nil", Function: agent.ToolCallFunction{Name: "x"}}, "", 20)
	require.NotEmpty(t, got)
	assert.Contains(t, got, "tool_name")
}

// TestToolResultErrorIsNotSentAsSuccess guards the common regression where a
// deterministic summary accidentally rewrites a tool failure as status 0.
func TestToolResultErrorIsNotSentAsSuccess(t *testing.T) {
	call := agent.ToolCall{ID: "e1", Function: agent.ToolCallFunction{Name: "webfetch"}}
	got := SummarizeToolResult(call, `{"status":1,"error":"connection refused"}`, 100)
	assert.Contains(t, got, `"status":"error"`)
	assert.Contains(t, got, "connection refused")
}
