package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	assert.NotContains(t, got, `"retrieval_ref"`,
		"a non-spilled summary must not advertise a fake retrieval path")
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

func TestSummarizeToolResult_plainJSONObjectIsOutcomeNotEmptyEnvelope(t *testing.T) {
	call := agent.ToolCall{ID: "json-1", Function: agent.ToolCallFunction{Name: "custom_tool", Arguments: `{"query":"id-42"}`}}
	full := `{"policy":"allow","count":2,"important_identifier":"id-42"}`
	got := SummarizeToolResult(call, full, 100)
	assert.Contains(t, got, `"status":"success"`)
	var decoded ToolResultSummary
	require.NoError(t, json.Unmarshal([]byte(got), &decoded))
	assert.Contains(t, decoded.Outcome, `"policy":"allow"`)
	assert.Contains(t, decoded.Outcome, "id-42")
	assert.NotContains(t, got, "no textual output")
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
	assert.Contains(t, got, "args=")
}

func TestIndexedToolArgumentsReachSummaries(t *testing.T) {
	msgs := []agent.Message{
		{Role: "user", Content: "anchor"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "call-args", Function: agent.ToolCallFunction{
				Name: "executor_bash", Arguments: `{"command":"go test ./internal/service"}`,
			},
		}}},
		{Role: "tool", ToolCallID: "call-args", Name: "executor_bash", Content: `{"status":0,"result":{"output":"ok","exit_code":0}}`},
	}
	summaries, _ := summarizeRetainedToolResults(msgs, 1, nil)
	assert.Contains(t, summaries["call-args"], "go test ./internal/service")
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

func TestCompactionService_SpillsReadableToolResultReference(t *testing.T) {
	workspace := t.TempDir()
	svc := &CompactionService{deps: SessionDeps{FS: &fakeFSStore{workspace: workspace}}}
	call := agent.ToolCall{
		ID:       "call-spill",
		Function: agent.ToolCallFunction{Name: "executor_bash", Arguments: `{"command":"cat big.txt"}`},
	}
	full := `{"status":0,"result":{"output":"` + strings.Repeat("x", 5000) + `","exit_code":0}}`

	ref, note := svc.spillRetainedToolResult(context.Background(), "user-1", "session-1", call, full)

	require.Empty(t, note)
	assert.True(t, strings.HasPrefix(ref, "tmp/compaction-outputs/"),
		"the retrieval reference must be a workspace-relative readable path")
	data, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(ref)))
	require.NoError(t, err)
	assert.Equal(t, full, string(data))

	summaryJSON := summarizeToolResult(call, full, 400, ref, note)
	var summary ToolResultSummary
	require.NoError(t, json.Unmarshal([]byte(summaryJSON), &summary))
	assert.Equal(t, ref, summary.RetrievalRef)
	assert.Contains(t, summary.RetrievalHint, "xizhi_read_file")
	assert.NotContains(t, summaryJSON, "tool_calls/")
}

func TestCompactionService_ToolResultSpillFailureOmitsFakeReference(t *testing.T) {
	svc := &CompactionService{deps: SessionDeps{FS: &fakeFSStore{}}}
	call := agent.ToolCall{ID: "call-nospill", Function: agent.ToolCallFunction{Name: "bash"}}
	full := strings.Repeat("x", 5000)

	ref, note := svc.spillRetainedToolResult(context.Background(), "user-1", "session-1", call, full)

	assert.Empty(t, ref)
	assert.Contains(t, note, "full result remains in persisted tool_calls")
	summaryJSON := summarizeToolResult(call, full, 400, ref, note)
	assert.NotContains(t, summaryJSON, `"retrieval_ref"`)
	assert.Contains(t, summaryJSON, `"retrieval_fail_note"`)
}

func TestSummarizeToolResult_retrievalReferenceSurvivesCJKBounds(t *testing.T) {
	call := agent.ToolCall{ID: "cjk-1", Function: agent.ToolCallFunction{Name: "unknown_tool"}}
	ref := "tmp/compaction-outputs/session/output.txt"
	summaryJSON := summarizeToolResult(call, strings.Repeat("界", 2000), 400, ref, "")

	var summary ToolResultSummary
	require.NoError(t, json.Unmarshal([]byte(summaryJSON), &summary),
		"bounded summaries must remain valid JSON")
	assert.Equal(t, ref, summary.RetrievalRef)
	assert.Contains(t, summary.RetrievalHint, "xizhi_read_file")
}
