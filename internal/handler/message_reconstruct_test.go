package handler

import (
	"testing"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessagesToAgentMessages_Empty(t *testing.T) {
	msgs, err := MessagesToAgentMessages(nil)
	require.NoError(t, err)
	assert.Nil(t, msgs)

	msgs, err = MessagesToAgentMessages([]model.Message{})
	require.NoError(t, err)
	assert.Nil(t, msgs)
}

func TestMessagesToAgentMessages_PlainTextConversation(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hello"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "Hi"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " there"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "Hi there"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_UserMessagesPreserved(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "first"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "one"},
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "second"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "two"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "one"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "two"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_MarkersIgnored(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hello"},
		{Agent: stream.AgentConfucius, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "hi"},
		{Agent: stream.AgentConfucius, Role: "", EventType: model.EventTypeAgentEnd},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_SubAgentEventsIgnored(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "do it"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "calling"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "42"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentEnd},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " done"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "calling done"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_SingleToolCallAndResult(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"results"}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "tc-1",
			Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"x"}`},
		}}},
		{Role: "tool", Content: "results", ToolCallID: "tc-1", Name: "web_search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_PlanUpdatedIgnoredButToolPairPreserved(t *testing.T) {
	planJSON := `{"revision":1,"steps":[{"step":"work","status":"in_progress"}]}`
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "go"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"plan-1","name":"update_plan","args":{"steps":[{"step":"work","status":"in_progress"}]}}`},
		{Agent: stream.AgentConfucius, Role: "", EventType: model.EventTypePlanUpdated, Content: planJSON},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"plan-1","output":{"revision":1,"steps":[{"step":"work","status":"in_progress"}]}}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, agent.Message{Role: "user", Content: "go"}, got[0])
	require.Len(t, got[1].ToolCalls, 1)
	assert.Equal(t, "plan-1", got[1].ToolCalls[0].ID)
	assert.Equal(t, "update_plan", got[1].ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"steps":[{"step":"work","status":"in_progress"}]}`, got[1].ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool", got[2].Role)
	assert.Equal(t, "plan-1", got[2].ToolCallID)
	assert.Equal(t, "update_plan", got[2].Name)
	assert.JSONEq(t, `{"revision":1,"steps":[{"step":"work","status":"in_progress"}]}`, got[2].Content)
}

func TestMessagesToAgentMessages_ParallelToolCalls(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search both"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-a","name":"web_search","args":{"q":"a"}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-b","name":"web_search","args":{"q":"b"}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-a","output":"A results"}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-b","output":"B results"}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search both"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{
			{ID: "tc-a", Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"a"}`}},
			{ID: "tc-b", Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"b"}`}},
		}},
		{Role: "tool", Content: "A results", ToolCallID: "tc-a", Name: "web_search"},
		{Role: "tool", Content: "B results", ToolCallID: "tc-b", Name: "web_search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_MissingToolResultOmitsCall(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		// No matching tool_result.
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_OldToolCallWithoutIDIsIgnored(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"name":"web_search","args":{"q":"x"}}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_ToolOutputStructuredJSON(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":{"items":[{"title":"R"}]}}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "tc-1",
			Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"x"}`},
		}}},
		{Role: "tool", Content: `{"items":[{"title":"R"}]}`, ToolCallID: "tc-1", Name: "web_search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_ReasoningOnly(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "solve"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "Let me think..."},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "solve"},
		{Role: "assistant", ReasoningContent: "Let me think..."},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_ReasoningAndContentMerged(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "solve"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "thinking "},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "more"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "Answer"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "solve"},
		{Role: "assistant", Content: "Answer", ReasoningContent: "thinking more"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_ReasoningFlushedBeforeToolCall(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "I need to search."},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"ok"}`},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", ReasoningContent: "I need to search."},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "tc-1",
			Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"x"}`},
		}}},
		{Role: "tool", Content: "ok", ToolCallID: "tc-1", Name: "web_search"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_SubAgentReasoningIgnored(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "do it"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "calling"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "sub thought"},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "42"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentEnd},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " done"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "calling done"},
	}
	assert.Equal(t, want, got)
}

func TestMessagesToAgentMessages_MixedUserAssistantToolTurns(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "first"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "thinking"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{}}`},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"ok"}`},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "done"},
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "second"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "thinking"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID:       "tc-1",
			Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{}`},
		}}},
		{Role: "tool", Content: "ok", ToolCallID: "tc-1", Name: "web_search"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "second"},
	}
	assert.Equal(t, want, got)
}

// TestMessagesToAgentMessages_InterleavedRunsRegroupCoherently (task 4.1):
// rows carrying run identity replay per (agent, run_id) — interleaved token
// fragments of two runs flush into two coherent assistant messages, in
// within-run arrival order, instead of merging across runs. The rows use a
// top-level agent name so they reach the grouping state machine (sub-agent
// rows are name-skipped from the LLM context by design; see the companion
// test below). Physically the row order and msg_index stay untouched — only
// the replay groups.
func TestMessagesToAgentMessages_InterleavedRunsRegroupCoherently(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "go"},
		// Two interleaved runs of the same agent, fragments alternating.
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "run1-alpha ", RunID: "call_x1"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "run2-omega ", RunID: "call_x2"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "run1-beta", RunID: "call_x1"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "run2-psi", RunID: "call_x2"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "go"},
		// Per-run fragments, within-run arrival order preserved.
		{Role: "assistant", Content: "run1-alpha run1-beta"},
		{Role: "assistant", Content: "run2-omega run2-psi"},
	}
	assert.Equal(t, want, got)
}

// TestMessagesToAgentMessages_NullRunIDGroupsByAgentAlone: rows without run
// identity keep the pre-change behavior — same-agent fragments merge across
// what would be run boundaries (this is exactly the historical garbling the
// capability fixes only for stamped rows).
func TestMessagesToAgentMessages_NullRunIDGroupsByAgentAlone(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "go"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "a "},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "b "},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeAgentStart, Content: "ignored"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "c"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "a b c"},
	}
	assert.Equal(t, want, got)
}

// TestMessagesToAgentMessages_InterleavedRunsToolPairingUnchanged (task 4.1):
// tool_call/tool_result pairing still matches by tool_call_id regardless of
// run stamps and result-arrival interleaving. Tool-call batching keys on the
// agent name (NOT the run composite): flushing on a run-key change would
// strand calls whose results arrive later under parallel dispatch, so runs
// interleave freely while pairing stays intact — identical to the pre-change
// shape for unstamped rows.
func TestMessagesToAgentMessages_InterleavedRunsToolPairingUnchanged(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "go"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall,
			Content: `{"tool_call_id":"tc-a","name":"web_search","args":{"q":"a"}}`, RunID: "call_x1"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall,
			Content: `{"tool_call_id":"tc-b","name":"web_search","args":{"q":"b"}}`, RunID: "call_x2"},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult,
			Content: `{"tool_call_id":"tc-b","output":"B results"}`, RunID: "call_x2"},
		{Agent: stream.AgentConfucius, Role: model.RoleTool, EventType: model.EventTypeToolResult,
			Content: `{"tool_call_id":"tc-a","output":"A results"}`, RunID: "call_x1"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	// Both calls batch into one assistant message (agent-keyed batching);
	// each call pairs with its own result by tool_call_id, results emitted in
	// call order.
	want := []agent.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{
			{ID: "tc-a", Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"a"}`}},
			{ID: "tc-b", Function: agent.ToolCallFunction{Name: "web_search", Arguments: `{"q":"b"}`}},
		}},
		{Role: "tool", Content: "A results", ToolCallID: "tc-a", Name: "web_search"},
		{Role: "tool", Content: "B results", ToolCallID: "tc-b", Name: "web_search"},
	}
	assert.Equal(t, want, got)
}

// TestMessagesToAgentMessages_SubAgentRunRowsStillNameSkipped: real sub-agent
// rows (Chongzhi/Liang carrying run ids) remain excluded from the LLM context
// — the run grouping changes how identity-carrying rows replay, never WHICH
// agents participate in the main prompt history.
func TestMessagesToAgentMessages_SubAgentRunRowsStillNameSkipped(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "go"},
		{Agent: model.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "sub output x1", RunID: "call_x1"},
		{Agent: model.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "sub output x2", RunID: "call_x2"},
		{Agent: stream.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "summary"},
	}

	got, err := MessagesToAgentMessages(prior)
	require.NoError(t, err)

	want := []agent.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "summary"},
	}
	assert.Equal(t, want, got)
}
