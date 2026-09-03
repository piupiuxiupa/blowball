package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfucius_UpdatePlanRunsBeforeParallelExecution(t *testing.T) {
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "plan-1", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"first","status":"in_progress"},{"step":"second","status":"in_progress"}],"explanation":"dispatch both"}`}},
				{ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"inspect","name":"Inspector"}`}},
				{ID: "spawn-2", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"verify","name":"Verifier"}`}},
			},
		},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
		SpawnSubagentTool: func(spec SubAgentSpec) (Agent, error) {
			return &fakeAgent{name: spec.Name, content: "result"}, nil
		},
	})

	events, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	planCall, planUpdated, planResult := -1, -1, -1
	firstChildActivity := len(events)
	for i, e := range events {
		if e.Type == stream.EventToolCall && e.Content == UpdatePlanTool {
			planCall = i
		}
		if e.Type == stream.EventPlanUpdated {
			planUpdated = i
			require.Equal(t, 1, e.Meta[stream.MetaRevision])

			var snapshot PlanSnapshot
			require.NoError(t, json.Unmarshal([]byte(e.Content), &snapshot))
			assert.Equal(t, 1, snapshot.Revision)
			require.Len(t, snapshot.Steps, 2)
			assert.Equal(t, PlanStatusInProgress, snapshot.Steps[0].Status)
			assert.Equal(t, PlanStatusInProgress, snapshot.Steps[1].Status)
		}
		if e.Type == stream.EventToolResult {
			if id, _ := e.Meta[stream.MetaToolCallID].(string); id == "plan-1" {
				planResult = i
			}
		}
		if e.Agent != c.Name() && (e.Type == stream.EventAgentStart || e.Type == stream.EventToken || e.Type == stream.EventToolCall) && i < firstChildActivity {
			firstChildActivity = i
		}
	}
	require.GreaterOrEqual(t, planCall, 0)
	require.GreaterOrEqual(t, planUpdated, 0)
	require.GreaterOrEqual(t, planResult, 0)
	assert.Less(t, planCall, planUpdated)
	assert.Less(t, planUpdated, planResult)
	assert.Less(t, planResult, firstChildActivity)

	last := client.lastRequest()
	var toolResult string
	for _, msg := range last.Messages {
		if msg.Role == "tool" && msg.ToolCallID == "plan-1" {
			toolResult = msg.Content
		}
	}
	var fedBack PlanSnapshot
	require.NoError(t, json.Unmarshal([]byte(toolResult), &fedBack))
	assert.Equal(t, 1, fedBack.Revision)
}

func TestConfucius_DuplicateUpdatePlanCallsAreRejectedBeforeMutation(t *testing.T) {
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "plan-1", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"first","status":"pending"}]}`}},
				{ID: "plan-2", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"second","status":"pending"}]}`}},
				{ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"still runs","name":"Worker"}`}},
			},
		},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	var childRuns int
	c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
		SpawnSubagentTool: func(SubAgentSpec) (Agent, error) {
			childRuns++
			return &fakeAgent{name: "Worker", content: "result"}, nil
		},
	})

	events, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	planUpdates := 0
	errorResults := 0
	for _, e := range events {
		if e.Type == stream.EventPlanUpdated {
			planUpdates++
		}
		if e.Type == stream.EventToolResult {
			id, _ := e.Meta[stream.MetaToolCallID].(string)
			if id == "plan-1" || id == "plan-2" {
				assert.Contains(t, e.Content, "only one plan update")
				errorResults++
			}
		}
	}
	assert.Equal(t, 0, planUpdates)
	assert.Equal(t, 2, errorResults)
	assert.Equal(t, 1, childRuns)

	_, exists := c.plan.current()
	assert.False(t, exists)
}

func TestConfucius_SubAgentStatusDoesNotAutomaticallyUpdatePlan(t *testing.T) {
	tests := []struct {
		name   string
		agent  func() *fakeAgent
		status string
	}{
		{name: "completed", agent: func() *fakeAgent { return &fakeAgent{name: "Worker", content: "result"} }, status: "completed"},
		{name: "capped", agent: func() *fakeAgent { return &fakeAgent{name: "Worker", content: "partial", hitCap: true} }, status: "capped"},
		{name: "error", agent: func() *fakeAgent { return &fakeAgent{name: "Worker", err: errors.New("boom")} }, status: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			child := tt.agent()
			client := newFake(
				fakeResponse{
					finishReason: "tool_calls",
					toolCalls: []ToolCall{
						{ID: "plan-1", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"work","status":"in_progress"}]}`}},
						{ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"work","name":"Worker"}`}},
					},
				},
				fakeResponse{content: "stopped without another plan update", finishReason: "stop"},
			)
			c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
				SpawnSubagentTool: func(SubAgentSpec) (Agent, error) { return child, nil },
			})

			events, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
			require.NoError(t, err)

			planUpdates := 0
			var spawnResult string
			for _, e := range events {
				if e.Type == stream.EventPlanUpdated {
					planUpdates++
				}
				if e.Type == stream.EventToolResult {
					if id, _ := e.Meta[stream.MetaToolCallID].(string); id == "spawn-1" {
						spawnResult = e.Content
					}
				}
			}
			require.Equal(t, 1, planUpdates, "only the explicit initial update")
			assert.Contains(t, spawnResult, "status: "+tt.status)

			current, ok := c.plan.current()
			require.True(t, ok)
			assert.Equal(t, 1, current.Revision)
			require.Len(t, current.Steps, 1)
			assert.Equal(t, PlanStatusInProgress, current.Steps[0].Status)
		})
	}
}

func TestConfucius_SubsequentUpdatePlanCanCompleteStep(t *testing.T) {
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "plan-1", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"work","status":"in_progress"}]}`}},
				{ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"work","name":"Worker"}`}},
			},
		},
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "plan-2", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"work","status":"completed"}],"explanation":"verified"}`}},
			},
		},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
		SpawnSubagentTool: func(SubAgentSpec) (Agent, error) {
			return &fakeAgent{name: "Worker", content: "result"}, nil
		},
	})

	_, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	current, ok := c.plan.current()
	require.True(t, ok)
	assert.Equal(t, 2, current.Revision)
	require.Len(t, current.Steps, 1)
	assert.Equal(t, PlanStatusCompleted, current.Steps[0].Status)
}

func TestConfucius_RootPlanIsNotInjectedIntoSubAgentContext(t *testing.T) {
	child := &fakeAgent{name: "Worker", content: "result"}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "plan-1", Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"private root step","status":"in_progress"},{"step":"sibling state","status":"pending"}],"explanation":"root-only"}`}},
				{ID: "spawn-1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"inspect one file","context":"Only inspect /tmp/a.txt and report the first line.","name":"Worker"}`}},
			},
		},
		fakeResponse{content: "done", finishReason: "stop"},
	)
	c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
		SpawnSubagentTool: func(SubAgentSpec) (Agent, error) { return child, nil },
	})

	_, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	require.Equal(t, 1, child.callCount())
	messages := child.calls[0].messages
	require.Len(t, messages, 1)
	assert.Equal(t, "user", messages[0].Role)
	assert.Equal(t, "Task: inspect one file\n\nContext:\nOnly inspect /tmp/a.txt and report the first line.", messages[0].Content)
	assert.NotContains(t, messages[0].Content, "private root step")
	assert.NotContains(t, messages[0].Content, "sibling state")
}

func TestSubAgent_CannotDispatchRootPlanUpdate(t *testing.T) {
	c := newTestConfuciusWithFactories(t, newFake(fakeResponse{content: "unused", finishReason: "stop"}), map[string]SubAgentFactory{
		SpawnSubagentTool: func(SubAgentSpec) (Agent, error) {
			return &fakeAgent{name: "Worker", content: "result"}, nil
		},
	})
	before, hadBefore := c.plan.current()
	require.False(t, hadBefore)

	child, err := NewSubAgent(SubAgentSpec{
		Name:         "Worker",
		SystemPrompt: "you are a child",
		ToolRegistry: tool.NewRegistry(),
		Turn:         testTurn(),
	}, nil)
	require.NoError(t, err)

	hub := stream.NewHub(8)
	defer hub.Close()
	result := child.dispatchOne(context.Background(), ToolCall{
		ID:       "plan-child",
		Function: ToolCallFunction{Name: UpdatePlanTool, Arguments: `{"steps":[{"step":"forge","status":"completed"}]}`},
	}, hub)
	assert.True(t, result.isError)
	assert.Contains(t, result.content, "restricted to Confucius")

	after, hadAfter := c.plan.current()
	require.False(t, hadAfter)
	assert.Equal(t, before, after)
}
