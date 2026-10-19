package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// TestMessagesToAgentMessages_FinalRowsReplayLiveWirePrefix is the prefix-cache
// oracle: after round-final persistence, recovered agent messages serialize to
// exactly the bytes the live agent loop would have sent for the completed turn.
func TestMessagesToAgentMessages_FinalRowsReplayLiveWirePrefix(t *testing.T) {
	info := testTurnPersistInfo()
	events := []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.ReasoningEvent(stream.AgentConfucius, "need facts"),
		stream.TokenEvent(stream.AgentConfucius, "checking"),
		stream.ToolCallEvent(stream.AgentConfucius, "c-b", "grep", json.RawMessage(`{ "q": "b" }`)),
		stream.ToolCallEvent(stream.AgentConfucius, "c-a", "grep", json.RawMessage(`{ "q": "a" }`)),
		stream.ToolResultEvent(stream.AgentConfucius, "c-a", "A result"),
		stream.ToolResultEvent(stream.AgentConfucius, "c-b", "B result"),
	}
	batch, _ := info.buildRoundFinalBatch(MergeEvents(events), 0, false, time.Now().UTC(),
		roundRenderOptions{terminal: true})

	live := []agent.Message{
		{Role: "user", Content: "hi"},
		{
			Role:             "assistant",
			Content:          "checking",
			ReasoningContent: "need facts",
			ToolCalls: []agent.ToolCall{
				{ID: "c-b", Function: agent.ToolCallFunction{Name: "grep", Arguments: `{ "q": "b" }`}},
				{ID: "c-a", Function: agent.ToolCallFunction{Name: "grep", Arguments: `{ "q": "a" }`}},
			},
		},
		{Role: "tool", Content: "B result", ToolCallID: "c-b", Name: "grep"},
		{Role: "tool", Content: "A result", ToolCallID: "c-a", Name: "grep"},
	}

	recovered, _, err := MessagesToAgentMessagesIndexed(batch.msgs, batch.calls)
	require.NoError(t, err)
	require.Equal(t, live, recovered)

	liveJSON, err := json.Marshal(live)
	require.NoError(t, err)
	recoveredJSON, err := json.Marshal(recovered)
	require.NoError(t, err)
	assert.Equal(t, string(liveJSON), string(recoveredJSON),
		"recovered history must preserve the live request prefix byte-for-byte")
}

// TestMessagesToAgentMessages_MixedLegacyAndFinalRowsKeepsProviderShape ensures
// a migrated session keeps legacy plain text intact while new narrated tool
// rounds replay in the provider-native assistant shape.
func TestMessagesToAgentMessages_MixedLegacyAndFinalRowsKeepsProviderShape(t *testing.T) {
	const trace = "trace-mixed"
	base := time.Unix(1_700_000_000, 0).UTC()
	prior := []model.Message{
		{MsgTime: base, Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "old question", TraceID: "old", MsgIndex: 0},
		{MsgTime: base.Add(time.Second), Agent: model.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "old answer", TraceID: "old", MsgIndex: 1},
		{MsgTime: base.Add(2 * time.Second), Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "new question", TraceID: trace, MsgIndex: 0},
		{MsgTime: base.Add(3 * time.Second), Agent: model.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeMessage, Content: "checking", ReasoningContent: "reason", TraceID: trace, MsgIndex: 1},
		{MsgTime: base.Add(3 * time.Second), Agent: model.AgentConfucius, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: "grep", TraceID: trace, MsgIndex: 2, ToolCallID: "c1"},
	}
	calls := []model.ToolCall{{
		TurnID: trace, ToolCallID: "c1", Name: "grep",
		Arguments: `{ "q": "x" }`, Result: "result", Status: model.ToolCallStatusCompleted,
	}}

	got, _, err := MessagesToAgentMessagesIndexed(prior, calls)
	require.NoError(t, err)
	require.Len(t, got, 5)
	assert.Equal(t, agent.Message{Role: "assistant", Content: "old answer"}, got[1])
	require.Len(t, got[3].ToolCalls, 1)
	assert.Equal(t, "checking", got[3].Content)
	assert.Equal(t, "reason", got[3].ReasoningContent)
	assert.Equal(t, `{ "q": "x" }`, got[3].ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool", got[4].Role)
}

// TestMessagesToAgentMessages_RecallPairAndNarratedToolAppendOnlyReplay covers
// the suspected cache-loss case directly: a synthetic recall pair remains
// append-only while a later ordinary narrated tool round replays unchanged.
func TestMessagesToAgentMessages_RecallPairAndNarratedToolAppendOnlyReplay(t *testing.T) {
	const (
		sessionID = "sess-recall-tool"
		traceRec  = "01920000-0000-7000-8000-000000000001"
		traceTool = "01920000-0000-7000-8000-000000000002"
	)
	ts := time.Unix(1_700_000_000, 0).UTC()
	res := recallResult(memory.RecallEntry{Key: "viking://u/mem/a", Line: "- [memory 87%] fact a"})
	inj := newRecallInjection(sessionID, traceRec, "question", res)
	recallPH, recallCall := inj.persistRows(sessionID, traceRec, ts)

	rows := []model.Message{
		recallPH,
		UserMessage(sessionID, traceRec, "question", ts),
		{
			SessionID: sessionID, MsgTime: ts.Add(time.Minute), Agent: model.AgentConfucius,
			MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeMessage,
			Content: "checking", ReasoningContent: "reason", TraceID: traceTool,
		},
		{
			SessionID: sessionID, MsgTime: ts.Add(time.Minute), Agent: model.AgentConfucius,
			MsgIndex: 2, Role: model.RoleAssistant, EventType: model.EventTypeToolCall,
			Content: "grep", TraceID: traceTool, ToolCallID: "c1",
		},
	}
	toolCall := model.ToolCall{
		SessionID: sessionID, TurnID: traceTool, ToolCallID: "c1", Name: "grep",
		Arguments: `{ "q": "x" }`, Result: "result", Status: model.ToolCallStatusCompleted,
	}

	live := append(inj.agentMessages(),
		agent.Message{Role: "user", Content: "question"},
		agent.Message{
			Role: "assistant", Content: "checking", ReasoningContent: "reason",
			ToolCalls: []agent.ToolCall{{
				ID: "c1", Function: agent.ToolCallFunction{Name: "grep", Arguments: `{ "q": "x" }`},
			}},
		},
		agent.Message{Role: "tool", Content: "result", ToolCallID: "c1", Name: "grep"},
	)
	recovered, err := MessagesToAgentMessages(rows, []model.ToolCall{recallCall, toolCall})
	require.NoError(t, err)
	require.Equal(t, live, recovered)

	liveJSON, err := json.Marshal(live)
	require.NoError(t, err)
	recoveredJSON, err := json.Marshal(recovered)
	require.NoError(t, err)
	assert.Equal(t, string(liveJSON), string(recoveredJSON))
}
