package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// newTestConfuciusWithFactories builds a Confucius whose sub-agents come from
// raw factories, for tests that need true per-invocation instances (as opposed
// to newTestConfucius, which hands out one shared fake per name).
func newTestConfuciusWithFactories(t *testing.T, client LLMClient, factories map[string]SubAgentFactory) *Confucius {
	t.Helper()
	c, err := NewConfucius(testConfuciusConfig(), client, nil, factories)
	require.NoError(t, err)
	return c
}

// concurrentSubAgent is a per-invocation fake whose instances coordinate via
// shared channels so concurrent-dispatch tests get deterministic
// happens-before edges: the "executor" task signals after setting its flags,
// the "waiter" task blocks until that signal before acting. Each instance
// owns its executedTool/hitCap flags — exactly the per-run state the
// per-invocation factories must isolate.
type concurrentSubAgent struct {
	name   string
	policy config.AgentRetryConfig
	// failFirst makes a waiter-task instance fail its first Run with a
	// transient error (then succeed on retry).
	failFirst bool
	// signal is closed by the executor-task instance after it sets its flags.
	signal chan struct{}
	// flags on THIS instance; read by the dispatcher after Run returns.
	executedTool bool
	hitCap       bool
	mu           sync.Mutex
	runCalls     int
}

const (
	concurrentExecutorTask = "collect from source A"
	concurrentWaiterTask   = "collect from source B"
)

func (a *concurrentSubAgent) Name() string           { return a.name }
func (a *concurrentSubAgent) SystemPrompt() string   { return "" }
func (a *concurrentSubAgent) RetryPolicy() config.AgentRetryConfig { return a.policy }
func (a *concurrentSubAgent) LastRunExecutedTool() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.executedTool
}
func (a *concurrentSubAgent) LastRunHitCap() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hitCap
}

func (a *concurrentSubAgent) Run(ctx context.Context, messages []Message, hub stream.EventHub) (string, Usage, *TurnBreakdown, error) {
	a.mu.Lock()
	a.runCalls++
	a.mu.Unlock()

	if !hub.SendCtx(ctx, stream.AgentStartEvent(a.name)) {
		return "", Usage{}, nil, ctx.Err()
	}
	task := ""
	if len(messages) > 0 {
		task = messages[0].Content
	}
	switch {
	case strings.Contains(task, "source A"): // the executor instance
		a.mu.Lock()
		a.executedTool = true // side-effect flag set DURING the run
		a.hitCap = true       // cap flag set during the run
		a.mu.Unlock()
		// The waiter may now act: it observes both flags set on this
		// instance (the happens-before edge the shared-instance bug broke).
		close(a.signal)
		hub.SendCtx(ctx, stream.TokenEvent(a.name, "A-result"))
		hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
		return "A_RESULT", Usage{TotalTokens: 3}, nil, nil
	default: // the waiter instance
		select {
		case <-a.signal:
		case <-ctx.Done():
			return "", Usage{}, nil, ctx.Err()
		}
		a.mu.Lock()
		calls := a.runCalls
		a.mu.Unlock()
		if a.failFirst && calls == 1 {
			hub.SendCtx(ctx, stream.AgentErrorEvent(a.name, transientErr.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
			return "", Usage{TotalTokens: 5}, nil, transientErr
		}
		hub.SendCtx(ctx, stream.TokenEvent(a.name, "B-result"))
		hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
		return "B_RESULT", Usage{TotalTokens: 4}, nil, nil
	}
}

func (a *concurrentSubAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runCalls
}

// parallelInvokeClient queues a Confucius script: round 1 dispatches two
// same-name invokes (x1=executor task, x2=waiter task), round 2 stops.
func parallelInvokeClient() *fakeLLMClient {
	return newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "run_x1", Function: ToolCallFunction{Name: ToolInvokeChongzhi,
					Arguments: `{"task":"collect from source A"}`}},
				{ID: "run_x2", Function: ToolCallFunction{Name: ToolInvokeChongzhi,
					Arguments: `{"task":"collect from source B"}`}},
			},
		},
		fakeResponse{content: "merged", finishReason: "stop"},
	)
}

// TestConfucius_ParallelSameInvoke_SideEffectFlagsIsolated (task 1.4):
// two concurrent invoke_chongzhi dispatches, X1 executes a tool and succeeds,
// X2 fails transiently AFTER X1's side-effect flag is set. X2's retry
// eligibility must be judged on ITS OWN flag (no tool executed → retried),
// not on X1's — and the retry must reuse X2's instance (no rebuild).
func TestConfucius_ParallelSameInvoke_SideEffectFlagsIsolated(t *testing.T) {
	defer goleak.VerifyNone(t)

	signal := make(chan struct{})
	var builds atomic.Int32
	instances := make([]*concurrentSubAgent, 0, 2)
	var instMu sync.Mutex
	factory := func() (Agent, error) {
		builds.Add(1)
		a := &concurrentSubAgent{
			name:      "Chongzhi",
			signal:    signal,
			failFirst: true,
			policy:    config.AgentRetryConfig{Enabled: true, MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		}
		instMu.Lock()
		instances = append(instances, a)
		instMu.Unlock()
		return a, nil
	}

	c := newTestConfuciusWithFactories(t, parallelInvokeClient(), map[string]SubAgentFactory{
		ToolInvokeChongzhi: factory,
		ToolInvokeLiang:    func() (Agent, error) { return &fakeAgent{name: "Liang"}, nil },
	})

	_, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	instMu.Lock()
	defer instMu.Unlock()
	require.Len(t, instances, 2, "each invocation must build its own instance")
	assert.Equal(t, int32(2), builds.Load(), "the retry must reuse X2's instance, not rebuild")

	var executor, waiter *concurrentSubAgent
	for _, a := range instances {
		if a.callCount() == 1 {
			executor = a // X1: one successful run
		} else {
			waiter = a // X2: failed once, retried once
		}
	}
	require.NotNil(t, executor, "exactly one instance must run once")
	require.NotNil(t, waiter, "exactly one instance must run twice")
	assert.Equal(t, 2, waiter.callCount(), "X2 must be retried despite X1 having executed a tool")
	assert.True(t, executor.LastRunExecutedTool(), "X1 keeps its own side-effect flag")
	assert.False(t, waiter.LastRunExecutedTool(), "X2's flag stays clean")
}

// TestConfucius_ParallelSameInvoke_RoundCapPerInvocation (task 1.4): only X1
// hits its round cap; X2 completes concurrently WITHOUT a cap. X1's
// propagation into usage.meta.round_capped must survive X2's concurrent run
// (under a shared instance, X2's Run-entry flag reset could erase it).
func TestConfucius_ParallelSameInvoke_RoundCapPerInvocation(t *testing.T) {
	defer goleak.VerifyNone(t)

	signal := make(chan struct{})
	var builds atomic.Int32
	factory := func() (Agent, error) {
		builds.Add(1)
		return &concurrentSubAgent{name: "Chongzhi", signal: signal}, nil
	}

	c := newTestConfuciusWithFactories(t, parallelInvokeClient(), map[string]SubAgentFactory{
		ToolInvokeChongzhi: factory,
		ToolInvokeLiang:    func() (Agent, error) { return &fakeAgent{name: "Liang"}, nil },
	})

	_, _, _, breakdown, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)
	assert.Equal(t, int32(2), builds.Load(), "each invocation must build its own instance")

	require.NotNil(t, breakdown)
	assert.True(t, breakdown.RoundCapped, "X1's cap must propagate even while X2 runs concurrently")
}

// TestConfucius_SubAgentEventsCarryRunIdentity (task 2.3): every event emitted
// by a sub-agent Run carries Meta.parent_tool_call_id equal to the invoking
// tool_call id, while Confucius's own events (lifecycle, tokens, the invoke
// tool_call/tool_result pair, done) never carry the key. Includes nil-meta
// events (agent_start/agent_end), which must be initialized and stamped.
func TestConfucius_SubAgentEventsCarryRunIdentity(t *testing.T) {
	defer goleak.VerifyNone(t)

	chongzhi := &fakeAgent{name: "Chongzhi", content: "C_RESULT", tokens: []string{"c1", "c2"}}
	liang := &fakeAgent{name: "Liang", content: "L_RESULT", tokens: []string{"l1"}}

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "run_x1", Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"t1"}`}},
				{ID: "run_x2", Function: ToolCallFunction{Name: ToolInvokeLiang, Arguments: `{"task":"t2"}`}},
			},
		},
		fakeResponse{content: "merged", finishReason: "stop", tokens: []string{"merged"}},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	})

	events, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)
	require.NotEmpty(t, events)

	subEventTypes := map[string]bool{}
	for _, e := range events {
		switch e.Agent {
		case "Chongzhi", "Liang":
			id, ok := e.Meta[stream.MetaParentToolCallID].(string)
			require.True(t, ok, "sub-agent %s event %s must carry %s", e.Agent, e.Type, stream.MetaParentToolCallID)
			assert.Contains(t, []string{"run_x1", "run_x2"}, id, "run id must be the invoking tool_call id")
			if e.Agent == "Chongzhi" {
				assert.Equal(t, "run_x1", id)
			} else {
				assert.Equal(t, "run_x2", id)
			}
			subEventTypes[e.Type] = true
		case "Confucius":
			_, has := e.Meta[stream.MetaParentToolCallID]
			assert.False(t, has, "Confucius's own %s event must NOT carry %s", e.Type, stream.MetaParentToolCallID)
		}
	}
	// The lifecycle/token surface is covered (nil-meta events included).
	for _, ty := range []string{stream.EventAgentStart, stream.EventToken, stream.EventAgentEnd} {
		assert.True(t, subEventTypes[ty], "expected sub-agent %s events to be stamped", ty)
	}
	// The dispatcher-emitted invoke pair flows on the raw hub: Confucius's
	// tool_call for run_x1/run_x2 and the matching tool_results exist untagged.
	sawInvokeCall, sawInvokeResult := false, false
	for _, e := range events {
		if e.Agent == "Confucius" && e.Type == stream.EventToolCall {
			if id, _ := e.Meta[stream.MetaToolCallID].(string); id == "run_x1" || id == "run_x2" {
				sawInvokeCall = true
			}
		}
		if e.Agent == "Confucius" && e.Type == stream.EventToolResult {
			if id, _ := e.Meta[stream.MetaToolCallID].(string); id == "run_x1" || id == "run_x2" {
				sawInvokeResult = true
			}
		}
	}
	assert.True(t, sawInvokeCall, "the invoke tool_call events must be emitted (untagged)")
	assert.True(t, sawInvokeResult, "the invoke tool_result events must be emitted (untagged)")
}

// TestConfucius_FactoryBuildFailureSurfacesError: a factory that fails builds
// yields an agent_error + error tool result fed back to Confucius, without
// retry (the failure is deterministic).
func TestConfucius_FactoryBuildFailureSurfacesError(t *testing.T) {
	defer goleak.VerifyNone(t)

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "run_x1", Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"t1"}`}},
			},
		},
		fakeResponse{content: "ok", finishReason: "stop"},
	)
	buildErr := errors.New("registry exploded")
	c := newTestConfuciusWithFactories(t, client, map[string]SubAgentFactory{
		ToolInvokeChongzhi: func() (Agent, error) { return nil, buildErr },
		ToolInvokeLiang:    func() (Agent, error) { return &fakeAgent{name: "Liang"}, nil },
	})

	events, _, _, _, err := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	require.NoError(t, err)

	var sawBuildError bool
	for _, e := range events {
		if e.Type == stream.EventAgentError && strings.Contains(e.Content, "registry exploded") {
			sawBuildError = true
		}
	}
	assert.True(t, sawBuildError, "factory build failure must surface as agent_error")

	last := client.lastRequest()
	var fedBack string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "run_x1" {
			fedBack = m.Content
		}
	}
	assert.Contains(t, fedBack, "registry exploded", "build failure must be fed back to Confucius as the tool result")
}
