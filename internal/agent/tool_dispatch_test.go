package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
)

type recordingHub struct {
	mu     sync.Mutex
	events []stream.StreamEvent
}

func (h *recordingHub) Send(e stream.StreamEvent) bool {
	return h.SendCtx(context.Background(), e)
}

func (h *recordingHub) SendCtx(_ context.Context, e stream.StreamEvent) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
	return true
}

func (h *recordingHub) snapshot() []stream.StreamEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]stream.StreamEvent{}, h.events...)
}

func testDispatchCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Function: ToolCallFunction{Name: name, Arguments: args}}
}

func TestConfuciusDispatchToolCalls_EmitsAllCallEventsInResponseOrder(t *testing.T) {
	slow := &fakeExecutor{name: "slow_tool", result: "slow result"}
	fast := &fakeExecutor{name: "fast_tool", result: "fast result"}
	reg := tool.NewRegistry()
	for _, fake := range []*fakeExecutor{slow, fast} {
		spec := &tool.ToolSpec{
			Name:           fake.name,
			Description:    "fake tool",
			ParametersJSON: json.RawMessage(`{"type":"object"}`),
			Execute:        fake.execute,
		}
		require.NoError(t, reg.Register(spec))
	}
	c, err := NewConfucius(config.AgentConfig{
		Name: "Confucius", SystemPrompt: "test", Tools: []string{slow.name, fast.name},
	}, nil, reg, testSpawnCoordinator(reg, func(SubAgentSpec) (Agent, error) {
		return nil, context.Canceled
	}), testTurn())
	require.NoError(t, err)

	hub := &recordingHub{}
	var round []Message
	total := Usage{}
	byAgent := map[string]Usage{}
	c.executeAndRecordToolCalls(context.Background(), []ToolCall{
		testDispatchCall("c-slow", slow.name, `{ "delay": true }`),
		testDispatchCall("c-plan", UpdatePlanTool, `{"steps":[{ "step": "x" }]}`),
		testDispatchCall("c-fast", fast.name, `{ "delay": false }`),
	}, hub, newRetryBudget(0), newTurnMeta(), &total, byAgent, &round)

	var callIDs []string
	for _, e := range hub.snapshot() {
		if e.Type != stream.EventToolCall {
			continue
		}
		id, _ := e.Meta[stream.MetaToolCallID].(string)
		callIDs = append(callIDs, id)
	}
	assert.Equal(t, []string{"c-slow", "c-plan", "c-fast"}, callIDs,
		"all placeholders must be emitted in LLM response order before any result")

	require.Len(t, round, 3)
	assert.Equal(t, []string{"c-slow", "c-plan", "c-fast"}, []string{
		round[0].ToolCallID, round[1].ToolCallID, round[2].ToolCallID,
	}, "model-visible tool results stay in response order")
}

func TestSubAgentDispatchToolCalls_EmitsCallEventsBeforeExecution(t *testing.T) {
	fake := &fakeExecutor{name: "fake_tool", result: "ok"}
	reg := newSubAgentRegistryWithFake(t, fake)
	sub, err := newGenericFromAgentConfig(testExecutionConfig(fake.name), nil, reg, testTurn())
	require.NoError(t, err)

	hub := &recordingHub{}
	var round []Message
	usage := Usage{}
	sub.executeAndRecordToolCalls(context.Background(), []ToolCall{
		testDispatchCall("c1", fake.name, `{ "q": "x" }`),
		testDispatchCall("c2", fake.name, `{ "q": "y" }`),
	}, hub, &round, &usage)

	events := hub.snapshot()
	require.GreaterOrEqual(t, len(events), 4)
	assert.Equal(t, stream.EventToolCall, events[0].Type)
	assert.Equal(t, stream.EventToolCall, events[1].Type)
	assert.Equal(t, "c1", events[0].Meta[stream.MetaToolCallID])
	assert.Equal(t, "c2", events[1].Meta[stream.MetaToolCallID])
	require.Len(t, round, 2)
	assert.Equal(t, "c1", round[0].ToolCallID)
	assert.Equal(t, "c2", round[1].ToolCallID)
}
