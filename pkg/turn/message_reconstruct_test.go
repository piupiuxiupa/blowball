package turn

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/model"
)

func toolTailRecallFixture() ([]model.Message, []model.ToolCall, []agent.Message) {
	rows := []model.Message{
		{Role: model.RoleUser, EventType: model.EventTypeMessage, TraceID: "turn-a", ClientMsgID: "turn-a:0", Content: "first question"},
		{Role: model.RoleAssistant, EventType: model.EventTypeMessage, TraceID: "turn-a"},
		{Role: model.RoleAssistant, EventType: model.EventTypeToolCall, TraceID: "turn-a", ToolCallID: "call-a", Content: "lookup"},
		{Role: model.RoleAssistant, EventType: model.EventTypeToolCall, TraceID: "turn-b", ClientMsgID: "turn-b:-1", ToolCallID: "recall-b", Content: "recall_memories"},
		{Role: model.RoleUser, EventType: model.EventTypeMessage, TraceID: "turn-b", ClientMsgID: "turn-b:0", Content: "next question"},
	}
	calls := []model.ToolCall{
		{TurnID: "turn-a", ToolCallID: "call-a", Name: "lookup", Arguments: `{ "q": 1 }`, Result: "A result", Status: model.ToolCallStatusCompleted},
		{TurnID: "turn-b", ToolCallID: "recall-b", Name: "recall_memories", Arguments: `{"entries":["memory-b"]}`, Result: "B result", Status: model.ToolCallStatusCompleted},
	}
	want := []agent.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-a", Function: agent.ToolCallFunction{Name: "lookup", Arguments: `{ "q": 1 }`}}}},
		{Role: "tool", ToolCallID: "call-a", Name: "lookup", Content: "A result"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "recall-b", Function: agent.ToolCallFunction{Name: "recall_memories", Arguments: `{"entries":["memory-b"]}`}}}},
		{Role: "tool", ToolCallID: "recall-b", Name: "recall_memories", Content: "B result"},
		{Role: "user", Content: "next question"},
	}
	return rows, calls, want
}

func TestMessagesToAgentMessagesIndexed_ToolTailTurnBoundary(t *testing.T) {
	tests := []struct {
		name             string
		content          string
		reasoning        string
		legacyTail       bool
		emptyResult      bool
		ordinaryNextTool bool
	}{
		{name: "tool-only tail"},
		{name: "narrated tool tail", content: "checking", reasoning: "reason"},
		{name: "legacy tool tail", legacyTail: true},
		{name: "empty completed result", emptyResult: true},
		{name: "ordinary next-turn tool", ordinaryNextTool: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, calls, want := toolTailRecallFixture()
			rows[1].Content, rows[1].ReasoningContent = tt.content, tt.reasoning
			want[1].Content, want[1].ReasoningContent = tt.content, tt.reasoning
			if tt.legacyTail {
				rows[1] = model.Message{
					Role: model.RoleAssistant, EventType: model.EventTypeToolCall, TraceID: "turn-a",
					Content: `{"tool_call_id":"call-a","name":"lookup","args":{"q":1}}`,
				}
				rows[2] = model.Message{
					Role: model.RoleTool, EventType: model.EventTypeToolResult, TraceID: "turn-a",
					Content: `{"tool_call_id":"call-a","output":"A result"}`,
				}
				want[1].ToolCalls[0].Function.Arguments = `{"q":1}`
			}
			if tt.emptyResult {
				calls[0].Result, want[2].Content = "", ""
			}
			if tt.ordinaryNextTool {
				rows[3].ToolCallID, rows[3].Content = "call-b", "lookup"
				calls[1].ToolCallID, calls[1].Name = "call-b", "lookup"
				want[3].ToolCalls[0].ID, want[3].ToolCalls[0].Function.Name = "call-b", "lookup"
				want[4].ToolCallID, want[4].Name = "call-b", "lookup"
			}

			got, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, []int{0, 2, 2, 3, 3, 4}, lastRow,
				"each tool unit must retain only its own canonical row attribution")
		})
	}
}

func TestMessagesToAgentMessagesIndexed_SameRoundParallelPlaceholders(t *testing.T) {
	rows, calls, want := toolTailRecallFixture()
	rows[3].TraceID, calls[1].TurnID = "turn-a", "turn-a"
	calls[0].Result, want[2].Content = "", ""
	want[1].ToolCalls = append(want[1].ToolCalls, want[3].ToolCalls...)

	got, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	assert.Equal(t, []agent.Message{want[0], want[1], want[2], want[4], want[5]}, got)
	assert.Equal(t, []int{0, 3, 2, 3, 4}, lastRow)
}

func TestMessagesToAgentMessagesIndexed_SameTurnSeparateFinalRounds(t *testing.T) {
	rows, calls, want := toolTailRecallFixture()
	rows[3].TraceID, calls[1].TurnID = "turn-a", "turn-a"
	rows = []model.Message{
		rows[0], rows[1], rows[2],
		{Role: model.RoleAssistant, EventType: model.EventTypeMessage, TraceID: "turn-a"},
		rows[3], rows[4],
	}

	got, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, []int{0, 2, 2, 4, 4, 5}, lastRow)
}

func TestMessagesToAgentMessagesIndexed_UnknownTurnKeepsExistingGrouping(t *testing.T) {
	rows, calls, want := toolTailRecallFixture()
	rows[3].TraceID, calls[1].TurnID = "", ""
	want[1].ToolCalls = append(want[1].ToolCalls, want[3].ToolCalls...)

	got, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	require.NoError(t, err)
	assert.Equal(t, []agent.Message{want[0], want[1], want[2], want[4], want[5]}, got)
	assert.Equal(t, []int{0, 3, 2, 3, 4}, lastRow)
}
