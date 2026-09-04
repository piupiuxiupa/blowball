package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureAgent struct {
	spec     SubAgentSpec
	messages []Message
	name     string
}

func (a *captureAgent) Name() string { return a.name }
func (a *captureAgent) SystemPrompt() string {
	return a.spec.SystemPrompt
}
func (a *captureAgent) RetryPolicy() config.AgentRetryConfig { return a.spec.Retry }
func (a *captureAgent) Run(_ context.Context, messages []Message, hub stream.EventHub) (string, Usage, *TurnBreakdown, error) {
	a.messages = append([]Message(nil), messages...)
	if !hub.SendCtx(context.Background(), stream.AgentStartEvent(a.name)) {
		return "", Usage{}, nil, context.Canceled
	}
	return "done", Usage{TotalTokens: 3}, nil, nil
}
func (a *captureAgent) LastRunMessages() []Message { return a.messages }

type fakeSnapshotStore struct {
	mu   sync.Mutex
	runs map[string]model.SubAgentRun
}

func (s *fakeSnapshotStore) UpsertSubAgentRun(_ context.Context, run model.SubAgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string]model.SubAgentRun{}
	}
	s.runs[run.AgentInstanceID] = run
	return nil
}

func (s *fakeSnapshotStore) GetSubAgentRun(_ context.Context, _, instanceID string) (model.SubAgentRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[instanceID]
	return run, ok, nil
}

func dynamicTestRegistry(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	for _, name := range names {
		name := name
		require.NoError(t, reg.Register(&tool.ToolSpec{
			Name: name, Description: name, ParametersJSON: []byte(`{}`),
			Execute: func(context.Context, json.RawMessage) (any, error) { return "ok", nil },
		}))
	}
	return reg
}

func dynamicTestConfig() config.SubAgentConfig {
	return config.SubAgentConfig{
		AgentConfig:      config.AgentConfig{Name: "Subagent", SystemPrompt: "generic"},
		MaxDepth:         2,
		MaxConcurrent:    4,
		MaxTotalPerTurn:  12,
		MaxSnapshotBytes: 1 << 20,
		Presets: map[string]config.SubAgentPresetConfig{
			"reviewer": {SystemPrompt: "review", Tools: []string{"read"}},
		},
	}
}

func TestSpawnCoordinator_ArgumentAndAuthorizationValidation(t *testing.T) {
	reg := dynamicTestRegistry(t, "read", "write")
	store := &fakeSnapshotStore{}
	var built int
	coordinator := newSpawnCoordinator(dynamicTestConfig(), func(SubAgentSpec) (Agent, error) {
		built++
		return &captureAgent{name: "subagent"}, nil
	}, store, newTurnMeta())
	coordinator.rootRegistry = reg
	coordinator.rootToolNames = []string{"read", "write"}

	cases := []struct {
		name       string
		args       string
		errContain string
	}{
		{"missing task", `{"context":"x"}`, `missing required field "task"`},
		{"blank task", `{"task":"  "}`, `missing required field "task"`},
		{"unknown field", `{"task":"x","extra":1}`, "unknown field"},
		{"tool expansion", `{"task":"x","tools":["admin"]}`, `unknown tools [admin]`},
		{"unknown preset", `{"task":"x","preset":"missing"}`, `unknown preset "missing"`},
		{"unknown resume", `{"task":"x","resume_agent_id":"w-missing"}`, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := coordinator.dispatch(WithSessionID(context.Background(), "session-1"), ToolCall{
				ID: "call_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: tc.args},
			}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read", "write"}})
			assert.True(t, result.isError)
			assert.Contains(t, result.content, tc.errContain)
		})
	}
	assert.Equal(t, 0, built)
}

func TestSpawnCoordinator_ToolNarrowingAndDefaultInheritance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  string
		tools []string
	}{
		{"narrowed", `{"task":"x","tools":["read"]}`, []string{"read"}},
		{"inherited", `{"task":"x"}`, []string{"read", "write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := dynamicTestRegistry(t, "read", "write")
			captured := make(chan *captureAgent, 1)
			coordinator := newSpawnCoordinator(dynamicTestConfig(), func(spec SubAgentSpec) (Agent, error) {
				a := &captureAgent{name: spec.Name, spec: spec}
				captured <- a
				return a, nil
			}, nil, newTurnMeta())
			coordinator.rootRegistry = reg
			coordinator.rootToolNames = []string{"read", "write"}

			result := coordinator.dispatch(context.Background(), ToolCall{
				ID: "call_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: tc.args},
			}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read", "write"}})
			require.False(t, result.isError)
			a := <-captured
			assert.Equal(t, tc.tools, a.spec.Tools)
			for _, name := range tc.tools {
				_, ok := a.spec.ToolRegistry.Get(name)
				assert.True(t, ok)
			}
			_, ok := a.spec.ToolRegistry.Get(SpawnSubagentTool)
			assert.False(t, ok, "spawn is synthetic and must not enter the registry")
			assert.Contains(t, result.content, "status: completed")
		})
	}
}

func TestTreeBudget_TotalAndConcurrency(t *testing.T) {
	total := newTreeBudget(2, 4, 1, 10*time.Millisecond)
	require.NoError(t, total.reserveTotal())
	require.Error(t, total.reserveTotal())
	total.releaseTotal()

	budget := newTreeBudget(2, 1, 0, 10*time.Millisecond)
	require.NoError(t, budget.reserveTotal())
	require.NoError(t, budget.acquireSlot(context.Background(), 0))
	require.Error(t, budget.acquireSlot(context.Background(), 0))
	budget.releaseSlot()
	require.NoError(t, budget.acquireSlot(context.Background(), 0))
	budget.releaseSlot()
	budget.releaseTotal()
}

func TestSpawnCoordinator_ResumeUsesSnapshotAndStableInstance(t *testing.T) {
	reg := dynamicTestRegistry(t, "read")
	store := &fakeSnapshotStore{}
	captured := make(chan *captureAgent, 2)
	coordinator := newSpawnCoordinator(dynamicTestConfig(), func(spec SubAgentSpec) (Agent, error) {
		a := &captureAgent{name: spec.Name, spec: spec}
		captured <- a
		return a, nil
	}, store, newTurnMeta())
	coordinator.rootRegistry = reg
	coordinator.rootToolNames = []string{"read"}
	ctx := WithSessionID(context.Background(), "session-1")

	first := coordinator.dispatch(ctx, ToolCall{
		ID: "run_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"first","name":"writer"}`},
	}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}})
	require.False(t, first.isError)
	a1 := <-captured
	require.Len(t, a1.messages, 1)

	store.mu.Lock()
	saved := store.runs[""]
	var count int
	for _, run := range store.runs {
		count++
		saved = run
	}
	store.mu.Unlock()
	require.Equal(t, 1, count)
	require.True(t, saved.ResumeEligible)

	second := coordinator.dispatch(ctx, ToolCall{
		ID: "run_2", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: fmt.Sprintf(`{"task":"continue","resume_agent_id":%q}`, saved.AgentInstanceID)},
	}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}})
	require.False(t, second.isError)
	assert.Equal(t, saved.AgentInstanceID, second.subAgentID)
	assert.Equal(t, saved.Name, second.subAgentName)
	a2 := <-captured
	require.GreaterOrEqual(t, len(a2.messages), 2)
	assert.Equal(t, "continue", a2.messages[len(a2.messages)-1].Content)

	saved.ResumeEligible = false
	require.NoError(t, store.UpsertSubAgentRun(ctx, saved))
	blocked := coordinator.dispatch(ctx, ToolCall{
		ID: "run_3", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: fmt.Sprintf(`{"task":"again","resume_agent_id":%q}`, saved.AgentInstanceID)},
	}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}})
	assert.True(t, blocked.isError)
	assert.Contains(t, blocked.content, "oversized or unreadable snapshot")
}

func TestSpawnCoordinator_NestedDispatchSharesIdentityAndBudget(t *testing.T) {
	reg := dynamicTestRegistry(t, "read")
	cfg := dynamicTestConfig()
	var coordinator *spawnCoordinator
	client := newFake(
		fakeResponse{finishReason: "tool_calls", toolCalls: []ToolCall{{ID: "grand_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"grand"}`}}},
			usage: Usage{TotalTokens: 2}},
		fakeResponse{content: "grand-done", finishReason: "stop", usage: Usage{TotalTokens: 3}},
		fakeResponse{content: "parent-done", finishReason: "stop", usage: Usage{TotalTokens: 5}},
	)
	coordinator = newSpawnCoordinator(cfg, func(spec SubAgentSpec) (Agent, error) {
		spec.Coordinator = coordinator
		spec.Turn = testTurn()
		return NewSubAgent(spec, client)
	}, nil, nil)
	coordinator.rootRegistry = reg
	coordinator.rootToolNames = []string{"read"}

	hub := stream.NewHub(16)
	result := coordinator.dispatch(context.Background(), ToolCall{
		ID: "root_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"parent"}`},
	}, hub, nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}})
	require.False(t, result.isError)
	assert.Contains(t, result.content, "status: completed")
	require.Equal(t, int64(2), coordinator.budget.total.Load())
	// Root and grandchild are separate invocations in shared metadata.
	invocations, _, _, _ := coordinator.meta.snapshot()
	assert.Len(t, invocations, 2)
}

func TestAppendSpawnResult_Statuses(t *testing.T) {
	completed := appendSpawnResult("answer", "w-abc", SubAgentStatusCompleted)
	assert.True(t, strings.HasSuffix(completed, "agent_id: w-abc\nstatus: completed\n"))
	assert.Equal(t, "answer\n\n--- subagent_result ---\nagent_id: w-abc\nstatus: completed\n", completed)
	assert.Contains(t, appendSpawnResult("x", "w-abc", SubAgentStatusCapped), "status: capped")
	assert.Contains(t, appendSpawnResult("x", "w-abc", SubAgentStatusError), "status: error")
}
