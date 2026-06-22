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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "Hi"},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " there"},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "one"},
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "second"},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "two"},
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
		{Agent: stream.AgentConfuse, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "hi"},
		{Agent: stream.AgentConfuse, Role: "", EventType: model.EventTypeAgentEnd},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "calling"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "42"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentEnd},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " done"},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"results"}`},
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

func TestMessagesToAgentMessages_ParallelToolCalls(t *testing.T) {
	prior := []model.Message{
		{Agent: model.AgentUser, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "search both"},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-a","name":"web_search","args":{"q":"a"}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-b","name":"web_search","args":{"q":"b"}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-a","output":"A results"}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-b","output":"B results"}`},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"name":"web_search","args":{"q":"x"}}`},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":{"items":[{"title":"R"}]}}`},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "Let me think..."},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "thinking "},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "more"},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "Answer"},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "I need to search."},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{"q":"x"}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"ok"}`},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "calling"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentStart},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeReasoning, Content: "sub thought"},
		{Agent: stream.AgentChongzhi, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "42"},
		{Agent: stream.AgentChongzhi, Role: "", EventType: model.EventTypeAgentEnd},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: " done"},
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
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "thinking"},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToolCall, Content: `{"tool_call_id":"tc-1","name":"web_search","args":{}}`},
		{Agent: stream.AgentConfuse, Role: model.RoleTool, EventType: model.EventTypeToolResult, Content: `{"tool_call_id":"tc-1","output":"ok"}`},
		{Agent: stream.AgentConfuse, Role: model.RoleAssistant, EventType: model.EventTypeToken, Content: "done"},
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
