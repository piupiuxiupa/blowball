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
		stream.AgentStartEvent(stream.AgentConfucius),
		stream.TokenEvent(stream.AgentConfucius, "Hello"),
		stream.AgentEndEvent(stream.AgentConfucius),
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

func TestMessageFromEvent_Reasoning(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()
	e := stream.ReasoningEvent(stream.AgentConfucius, "analyzing...")
	msg, err := MessageFromEvent(e, "sess-1", "trace-1", 1, msgTime)
	require.NoError(t, err)

	assert.Equal(t, model.EventTypeReasoning, msg.EventType)
	assert.Equal(t, model.RoleAssistant, msg.Role)
	assert.Equal(t, "analyzing...", msg.Content)
	assert.Equal(t, stream.AgentConfucius, msg.Agent)
}

func TestMergeEvents_Reasoning(t *testing.T) {
	tests := []struct {
		name     string
		in       []stream.StreamEvent
		expected []stream.StreamEvent
	}{
		{
			name: "pure reasoning sequence is merged",
			in: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "Let "),
				stream.ReasoningEvent(stream.AgentConfucius, "me "),
				stream.ReasoningEvent(stream.AgentConfucius, "think"),
			},
			expected: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "Let me think"),
			},
		},
		{
			name: "reasoning and token are not merged",
			in: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "thinking"),
				stream.TokenEvent(stream.AgentConfucius, "answer"),
			},
			expected: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "thinking"),
				stream.TokenEvent(stream.AgentConfucius, "answer"),
			},
		},
		{
			name: "different agents break reasoning merge",
			in: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "A"),
				stream.ReasoningEvent(stream.AgentLiang, "B"),
			},
			expected: []stream.StreamEvent{
				stream.ReasoningEvent(stream.AgentConfucius, "A"),
				stream.ReasoningEvent(stream.AgentLiang, "B"),
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
			in:   []stream.StreamEvent{stream.TokenEvent(stream.AgentConfucius, "hi")},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "hi"),
			},
		},
		{
			name: "pure token sequence is merged",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "H"),
				stream.TokenEvent(stream.AgentConfucius, "e"),
				stream.TokenEvent(stream.AgentConfucius, "l"),
				stream.TokenEvent(stream.AgentConfucius, "l"),
				stream.TokenEvent(stream.AgentConfucius, "o"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "Hello"),
			},
		},
		{
			name: "lifecycle events break token merge",
			in: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfucius),
				stream.TokenEvent(stream.AgentConfucius, "He"),
				stream.TokenEvent(stream.AgentConfucius, "llo"),
				stream.AgentEndEvent(stream.AgentConfucius),
			},
			expected: []stream.StreamEvent{
				stream.AgentStartEvent(stream.AgentConfucius),
				stream.TokenEvent(stream.AgentConfucius, "Hello"),
				stream.AgentEndEvent(stream.AgentConfucius),
			},
		},
		{
			name: "different agents are not merged",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "A"),
				stream.AgentStartEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentLiang, "B"),
				stream.AgentEndEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentConfucius, "C"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "A"),
				stream.AgentStartEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentLiang, "B"),
				stream.AgentEndEvent(stream.AgentLiang),
				stream.TokenEvent(stream.AgentConfucius, "C"),
			},
		},
		{
			name: "tool calls remain independent",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "before"),
				stream.ToolCallEvent(stream.AgentConfucius, "tc-1", "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfucius, "after"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "before"),
				stream.ToolCallEvent(stream.AgentConfucius, "tc-1", "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfucius, "after"),
			},
		},
		{
			name: "sub-agent hand-off preserves order",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "call"),
				stream.ToolCallEvent(stream.AgentConfucius, "tc-2", "invoke_chongzhi", map[string]any{"task": "compute"}),
				stream.AgentStartEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentChongzhi, "42"),
				stream.AgentEndEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentConfucius, "done"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "call"),
				stream.ToolCallEvent(stream.AgentConfucius, "tc-2", "invoke_chongzhi", map[string]any{"task": "compute"}),
				stream.AgentStartEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentChongzhi, "42"),
				stream.AgentEndEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentConfucius, "done"),
			},
		},
		{
			name: "agent error breaks merge",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "abc"),
				stream.AgentErrorEvent(stream.AgentConfucius, "boom", "err"),
				stream.TokenEvent(stream.AgentConfucius, "def"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfucius, "abc"),
				stream.AgentErrorEvent(stream.AgentConfucius, "boom", "err"),
				stream.TokenEvent(stream.AgentConfucius, "def"),
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

// taggedToken builds a Chongzhi token event stamped with a run id, the shape
// the dispatch layer's run tagger emits during parallel same-name invocations.
func taggedToken(runID, content string) stream.StreamEvent {
	e := stream.TokenEvent(stream.AgentChongzhi, content)
	e.Meta = map[string]any{stream.MetaParentToolCallID: runID}
	return e
}

// TestMergeEvents_RunIDChangeIsBoundary: interleaved tokens of concurrent
// same-name invocations never merge across runs — each run's fragments stay
// attributable — while fragments of the SAME run still merge and untagged
// (top-level) events keep the pre-change adjacent-merge behavior.
func TestMergeEvents_RunIDChangeIsBoundary(t *testing.T) {
	events := []stream.StreamEvent{
		taggedToken("x1", "run1-a "),
		taggedToken("x2", "run2-a "), // same agent+type, DIFFERENT run → boundary
		taggedToken("x1", "run1-b"),  // back to x1 → new fragment
		taggedToken("x1", "run1-c"),  // same run → merges with previous
	}

	merged := MergeEvents(events)
	require.Len(t, merged, 3, "run-id changes must break adjacency; same-run fragments merge")
	assert.Equal(t, "run1-a ", merged[0].Content)
	assert.Equal(t, "x1", merged[0].Meta[stream.MetaParentToolCallID])
	assert.Equal(t, "run2-a ", merged[1].Content)
	assert.Equal(t, "x2", merged[1].Meta[stream.MetaParentToolCallID])
	assert.Equal(t, "run1-brun1-c", merged[2].Content)
	assert.Equal(t, "x1", merged[2].Meta[stream.MetaParentToolCallID])
}

// TestMessageFromEvent_RunIDCopiedFromMeta: the persisted row carries the
// event's run identity; events without one persist an empty RunID (NULL at
// the store layer).
func TestMessageFromEvent_RunIDCopiedFromMeta(t *testing.T) {
	msgTime := time.Unix(1_700_000_000, 0).UTC()

	tagged := taggedToken("call_x1", "hello")
	m, err := MessageFromEvent(tagged, "sess-1", "trace-1", 1, msgTime)
	require.NoError(t, err)
	assert.Equal(t, "call_x1", m.RunID)

	plain := stream.TokenEvent(stream.AgentConfucius, "hello")
	m, err = MessageFromEvent(plain, "sess-1", "trace-1", 2, msgTime)
	require.NoError(t, err)
	assert.Empty(t, m.RunID, "top-level events persist no run identity")
}
