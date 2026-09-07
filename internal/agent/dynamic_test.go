package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	mu        sync.Mutex
	instances map[string]model.SubAgentInstance
	runs      map[string][]model.SubAgentRun
}

func (s *fakeSnapshotStore) AppendSubAgentRun(_ context.Context, instance model.SubAgentInstance, run model.SubAgentRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instances == nil {
		s.instances = map[string]model.SubAgentInstance{}
	}
	if s.runs == nil {
		s.runs = map[string][]model.SubAgentRun{}
	}
	key := instance.SessionID + "\x00" + instance.AgentInstanceID
	current, ok := s.instances[key]
	if !ok {
		if run.PreviousRunID != "" {
			return model.ErrSubAgentChainConflict
		}
		instance.LatestRunID = run.RunID
		s.instances[key] = instance
		s.runs[key] = append(s.runs[key], run)
		return nil
	}
	if current.LatestRunID != run.RunID && current.LatestRunID != run.PreviousRunID {
		return model.ErrSubAgentChainConflict
	}
	instance.LatestRunID = run.RunID
	s.instances[key] = instance
	for i := range s.runs[key] {
		if s.runs[key][i].RunID == run.RunID {
			if s.runs[key][i].AgentInstanceID != run.AgentInstanceID || s.runs[key][i].RunNo != run.RunNo {
				return fmt.Errorf("run identity collision: %s", run.RunID)
			}
			s.runs[key][i] = run
			return nil
		}
	}
	s.runs[key] = append(s.runs[key], run)
	return nil
}

func (s *fakeSnapshotStore) GetSubAgentHistory(_ context.Context, sessionID, instanceID string) (model.SubAgentHistory, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionID + "\x00" + instanceID
	instance, ok := s.instances[key]
	if !ok {
		return model.SubAgentHistory{}, false, nil
	}
	return model.SubAgentHistory{Instance: instance, Runs: append([]model.SubAgentRun(nil), s.runs[key]...)}, true, nil
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

	history, ok, err := store.GetSubAgentHistory(ctx, "session-1", first.subAgentID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, history.Runs, 1)
	saved := history.Runs[0]
	assert.Equal(t, "run_1", saved.RunID)
	assert.Equal(t, model.SubAgentSnapshotDelta, saved.SnapshotKind)
	assert.Equal(t, 0, saved.BaseMessageCount)
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
	history, ok, err = store.GetSubAgentHistory(ctx, "session-1", saved.AgentInstanceID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, history.Runs, 2)
	assert.NotContains(t, string(history.Runs[1].MessagesJSON), `"first"`, "resume rows store only their delta")

	history, ok, err = store.GetSubAgentHistory(ctx, "session-1", saved.AgentInstanceID)
	require.NoError(t, err)
	require.True(t, ok)
	history.Runs[len(history.Runs)-1].ResumeEligible = false
	store.mu.Lock()
	store.runs["session-1\x00"+saved.AgentInstanceID] = history.Runs
	store.mu.Unlock()
	blocked := coordinator.dispatch(ctx, ToolCall{
		ID: "run_3", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: fmt.Sprintf(`{"task":"again","resume_agent_id":%q}`, saved.AgentInstanceID)},
	}, stream.NewHub(4), nil, spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}})
	assert.True(t, blocked.isError)
	assert.Contains(t, blocked.content, "oversized or unreadable snapshot")
}

func TestMessagesFromRunChain_LegacyBaselineAndDelta(t *testing.T) {
	messages, err := messagesFromRunChain([]model.SubAgentRun{
		{
			RunID: "legacy", RunNo: 1, SnapshotKind: model.SubAgentSnapshotLegacyFull,
			ContextMessageCount: 2, MessagesJSON: []byte(`[
				{"role":"system","content":"system"},
				{"role":"user","content":"first"},
				{"role":"assistant","content":"answer"}
			]`),
		},
		{
			RunID: "next", PreviousRunID: "legacy", RunNo: 2,
			SnapshotKind: model.SubAgentSnapshotDelta, BaseMessageCount: 2,
			ContextMessageCount: 4, MessagesJSON: []byte(`[
				{"role":"user","content":"continue"},
				{"role":"assistant","content":"continued"}
			]`),
		},
	})
	require.NoError(t, err)
	require.Len(t, messages, 4)
	assert.Equal(t, "first", messages[0].Content)
	assert.Equal(t, "answer", messages[1].Content)
	assert.Equal(t, "continue", messages[2].Content)
	assert.Equal(t, "continued", messages[3].Content)
}

func TestMessagesFromRunChain_RejectsBrokenChain(t *testing.T) {
	base := model.SubAgentRun{
		RunID: "one", RunNo: 1, SnapshotKind: model.SubAgentSnapshotDelta,
		ContextMessageCount: 1, MessagesJSON: []byte(`[{"role":"user","content":"first"}]`),
	}
	badCounter := base
	badCounter.RunID, badCounter.RunNo = "two", 2
	badCounter.PreviousRunID = "one"
	badCounter.BaseMessageCount = 9
	badCounter.ContextMessageCount = 10
	badCounter.MessagesJSON = []byte(`[{"role":"user","content":"second"}]`)
	_, err := messagesFromRunChain([]model.SubAgentRun{base, badCounter})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base count")

	badOrder := badCounter
	badOrder.BaseMessageCount = 1
	badOrder.ContextMessageCount = 1
	badOrder.PreviousRunID = "missing"
	_, err = messagesFromRunChain([]model.SubAgentRun{base, badOrder})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predecessor")
}

func TestSpawnCoordinator_ConcurrentResumeCannotFork(t *testing.T) {
	reg := dynamicTestRegistry(t, "read")
	store := &fakeSnapshotStore{}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var initial atomic.Bool
	newAgent := func(spec SubAgentSpec) (Agent, error) {
		if initial.CompareAndSwap(false, true) {
			return &captureAgent{name: spec.Name, spec: spec}, nil
		}
		entered <- struct{}{}
		<-release
		return &captureAgent{name: spec.Name, spec: spec}, nil
	}
	coordinator := newSpawnCoordinator(dynamicTestConfig(), newAgent, store, newTurnMeta())
	coordinator.rootRegistry = reg
	coordinator.rootToolNames = []string{"read"}
	ctx := WithSessionID(context.Background(), "session-race")
	parent := spawnParent{agentName: "Confucius", registry: reg, toolNames: []string{"read"}}

	first := coordinator.dispatch(ctx, ToolCall{
		ID: "call_1", Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"first"}`},
	}, stream.NewHub(4), nil, parent)
	require.False(t, first.isError)

	results := make(chan toolResult, 2)
	for _, runID := range []string{"call_2a", "call_2b"} {
		go func(id string) {
			results <- coordinator.dispatch(ctx, ToolCall{
				ID: id,
				Function: ToolCallFunction{
					Name:      SpawnSubagentTool,
					Arguments: fmt.Sprintf(`{"task":"continue","resume_agent_id":%q}`, first.subAgentID),
				},
			}, stream.NewHub(4), nil, parent)
		}(runID)
	}
	<-entered
	<-entered
	close(release)
	r1, r2 := <-results, <-results
	assert.NotEqual(t, r1.isError, r2.isError, "exactly one concurrent resume may win")
	assert.Contains(t, r1.content+r2.content, "resumed concurrently")

	history, ok, err := store.GetSubAgentHistory(ctx, "session-race", first.subAgentID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, history.Runs, 2, "only one resume may append to the chain")
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
