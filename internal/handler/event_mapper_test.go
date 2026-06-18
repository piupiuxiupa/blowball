package handler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

func TestUserMessage(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()
	msg := UserMessage("sess-1", "trace-1", "hello", msgTime)

	assert.Equal(t, "sess-1", msg.SessionID)
	assert.Equal(t, "trace-1", msg.TraceID)
	assert.Equal(t, "hello", msg.Content)
	assert.Equal(t, msgTime, msg.MsgTime)
	assert.Equal(t, model.AgentUser, msg.Agent)
	assert.Equal(t, model.RoleUser, msg.Role)
	assert.Equal(t, model.EventTypeMessage, msg.EventType)
	assert.Equal(t, 0, msg.MsgIndex)
}

func TestUserMessage_PrependToAssistantBatch_Ordering(t *testing.T) {
	userTime := time.Unix(1_700_000_000, 0).UTC()
	assistantTime := userTime.Add(time.Second)

	userMsg := UserMessage("sess-1", "trace-1", "hi", userTime)

	events := []stream.StreamEvent{
		stream.AgentStartEvent(stream.AgentConfuse),
		stream.TokenEvent(stream.AgentConfuse, "Hello"),
		stream.AgentEndEvent(stream.AgentConfuse),
	}
	merged := MergeEvents(events)
	require.Len(t, merged, 3)

	msgs := make([]model.Message, 0, len(merged)+1)
	msgs = append(msgs, userMsg)
	for i, e := range merged {
		m, err := MessageFromEvent(e, "sess-1", "trace-1", i+1, assistantTime)
		require.NoError(t, err)
		msgs = append(msgs, m)
	}

	require.Len(t, msgs, 4)
	assert.Equal(t, model.AgentUser, msgs[0].Agent)
	assert.Equal(t, 0, msgs[0].MsgIndex)
	assert.Equal(t, userTime, msgs[0].MsgTime)

	for i, m := range msgs[1:] {
		assert.Equal(t, i+1, m.MsgIndex)
		assert.Equal(t, assistantTime, m.MsgTime)
	}
	assert.Equal(t, model.EventTypeAgentStart, msgs[1].EventType)
	assert.Equal(t, model.EventTypeToken, msgs[2].EventType)
	assert.Equal(t, model.EventTypeAgentEnd, msgs[3].EventType)
}

func TestMergeEvents(t *testing.T) {
	tests := []struct {
		name     string
		in       []stream.StreamEvent
		expected []stream.StreamEvent
	}{
		{
			name:     "empty input",
			in:       nil,
			expected: nil,
		},
		{
			name: "single event",
			in:   []stream.StreamEvent{stream.TokenEvent(stream.AgentConfuse, "hi")},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "hi"),
			},
		},
		{
			name: "pure token sequence is merged",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "H"),
				stream.TokenEvent(stream.AgentConfuse, "e"),
				stream.TokenEvent(stream.AgentConfuse, "l"),
				stream.TokenEvent(stream.AgentConfuse, "l"),
				stream.TokenEvent(stream.AgentConfuse, "o"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "Hello"),
			},
		},
		{
			name: "lifecycle events break token merge",
			in: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfuse),
				stream.TokenEvent(stream.AgentConfuse, "He"),
				stream.TokenEvent(stream.AgentConfuse, "llo"),
				stream.AgentEndEvent(stream.AgentConfuse),
			},
			expected: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfuse),
				stream.TokenEvent(stream.AgentConfuse, "Hello"),
				stream.AgentEndEvent(stream.AgentConfuse),
			},
		},
		{
			name: "different agents are not merged",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "A"),
				stream.AgentStartEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentLiang, "B"),
				stream.AgentEndEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentConfuse, "C"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "A"),
				stream.AgentStartEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentLiang, "B"),
				stream.AgentEndEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentConfuse, "C"),
			},
		},
		{
			name: "tool calls remain independent",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "before"),
				stream.ToolCallEvent(stream.AgentConfuse, "tc-1", "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfuse, "after"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "before"),
				stream.ToolCallEvent(stream.AgentConfuse, "tc-1", "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfuse, "after"),
			},
		},
		{
			name: "sub-agent hand-off preserves order",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "call"),
				stream.ToolCallEvent(stream.AgentConfuse, "tc-2", "invoke_chongzhi", map[string]any{"task": "compute"}),
				stream.AgentStartEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentChongzhi, "42"),
				stream.AgentEndEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentConfuse, "done"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "call"),
				stream.ToolCallEvent(stream.AgentConfuse, "tc-2", "invoke_chongzhi", map[string]any{"task": "compute"}),
				stream.AgentStartEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentChongzhi, "42"),
				stream.AgentEndEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentConfuse, "done"),
			},
		},
		{
			name: "agent error breaks merge",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "abc"),
				stream.AgentErrorEvent(stream.AgentConfuse, "boom", "err"),
				stream.TokenEvent(stream.AgentConfuse, "def"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "abc"),
				stream.AgentErrorEvent(stream.AgentConfuse, "boom", "err"),
				stream.TokenEvent(stream.AgentConfuse, "def"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeEvents(tc.in)
			assert.Equal(t, tc.expected, got)
		})
	}
}
