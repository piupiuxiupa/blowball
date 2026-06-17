package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lush/blowball/internal/stream"
)

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
				stream.ToolCallEvent(stream.AgentConfuse, "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfuse, "after"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "before"),
				stream.ToolCallEvent(stream.AgentConfuse, "web_search", map[string]any{"q": "x"}),
				stream.TokenEvent(stream.AgentConfuse, "after"),
			},
		},
		{
			name: "sub-agent hand-off preserves order",
			in: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "call"),
				stream.ToolCallEvent(stream.AgentConfuse, "invoke_chongzhi", map[string]any{"task": "compute"}),
				stream.AgentStartEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentChongzhi, "42"),
				stream.AgentEndEvent(stream.AgentChongzhi),
				stream.TokenEvent(stream.AgentConfuse, "done"),
			},
			expected: []stream.StreamEvent{
				stream.TokenEvent(stream.AgentConfuse, "call"),
				stream.ToolCallEvent(stream.AgentConfuse, "invoke_chongzhi", map[string]any{"task": "compute"}),
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
