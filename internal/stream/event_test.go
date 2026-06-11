package stream

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventConstructors verifies each helper constructor produces the correct
// Type/Agent/Content/Meta fields.
func TestEventConstructors(t *testing.T) {
	t.Run("TokenEvent", func(t *testing.T) {
		e := TokenEvent(AgentConfuse, "hello")
		assert.Equal(t, EventToken, e.Type)
		assert.Equal(t, AgentConfuse, e.Agent)
		assert.Equal(t, "hello", e.Content)
		assert.Nil(t, e.Meta)
	})

	t.Run("AgentStartEvent", func(t *testing.T) {
		e := AgentStartEvent(AgentChongzhi)
		assert.Equal(t, EventAgentStart, e.Type)
		assert.Equal(t, AgentChongzhi, e.Agent)
		assert.Empty(t, e.Content)
		assert.Nil(t, e.Meta)
	})

	t.Run("AgentEndEvent", func(t *testing.T) {
		e := AgentEndEvent(AgentLiang)
		assert.Equal(t, EventAgentEnd, e.Type)
		assert.Equal(t, AgentLiang, e.Agent)
		assert.Empty(t, e.Content)
		assert.Nil(t, e.Meta)
	})

	t.Run("AgentErrorEvent", func(t *testing.T) {
		e := AgentErrorEvent(AgentChongzhi, "boom", "E_TIMEOUT")
		assert.Equal(t, EventAgentError, e.Type)
		assert.Equal(t, AgentChongzhi, e.Agent)
		assert.Equal(t, "boom", e.Content)
		require.NotNil(t, e.Meta)
		assert.Equal(t, "E_TIMEOUT", e.Meta[MetaCode])
	})

	t.Run("ToolCallEvent", func(t *testing.T) {
		args := map[string]any{"path": "/a/b", "mode": "rw"}
		e := ToolCallEvent(AgentConfuse, "read_file", args)
		assert.Equal(t, EventToolCall, e.Type)
		assert.Equal(t, AgentConfuse, e.Agent)
		assert.Equal(t, "read_file", e.Content)
		require.NotNil(t, e.Meta)

		// args stored as json.RawMessage so it serializes inline as a nested object.
		raw, ok := e.Meta[MetaArgs].(json.RawMessage)
		require.True(t, ok, "args should be json.RawMessage, got %T", e.Meta[MetaArgs])
		var got map[string]any
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Equal(t, "/a/b", got["path"])
		assert.Equal(t, "rw", got["mode"])
	})

	t.Run("ToolCallEvent nil args", func(t *testing.T) {
		e := ToolCallEvent(AgentConfuse, "noop", nil)
		require.NotNil(t, e.Meta)
		_, present := e.Meta[MetaArgs]
		assert.False(t, present, "nil args must not populate Meta[args]")
	})

	t.Run("DoneEvent with usage", func(t *testing.T) {
		usage := map[string]any{
			"total_tokens": 1234,
			"agents": map[string]any{
				AgentConfuse: map[string]any{"prompt": 100, "completion": 200},
			},
		}
		e := DoneEvent(usage)
		assert.Equal(t, EventDone, e.Type)
		assert.Empty(t, e.Agent)
		require.NotNil(t, e.Meta)
		assert.Equal(t, usage, e.Meta[MetaUsage])
	})

	t.Run("DoneEvent nil usage", func(t *testing.T) {
		e := DoneEvent(nil)
		assert.Equal(t, EventDone, e.Type)
		require.NotNil(t, e.Meta)
		_, present := e.Meta[MetaUsage]
		assert.False(t, present)
	})
}

// TestStreamEvent_JSONShape pins the JSON shape that handlers/frontends depend
// on so a future refactor cannot silently rename fields.
func TestStreamEvent_JSONShape(t *testing.T) {
	e := StreamEvent{
		Type:    EventToken,
		Agent:   AgentConfuse,
		Content: "hi",
		Meta:    map[string]any{"k": "v"},
	}
	b, err := json.Marshal(e)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, EventToken, got["type"])
	assert.Equal(t, AgentConfuse, got["agent"])
	assert.Equal(t, "hi", got["content"])
	assert.Equal(t, "v", got["meta"].(map[string]any)["k"])
}

// TestStreamEvent_OmitEmptyMeta ensures events with nil Meta do not serialize a
// "meta": null field — keeping payloads compact for the high-frequency token case.
func TestStreamEvent_OmitEmptyMeta(t *testing.T) {
	e := TokenEvent(AgentConfuse, "x")
	b, err := json.Marshal(e)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, present := got["meta"]
	assert.False(t, present, "nil meta should be omitted from JSON")
}
