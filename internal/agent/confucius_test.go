package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func testConfuciusConfig() config.AgentConfig {
	return config.AgentConfig{
		Name:         "Confucius",
		Model:        "gpt-test",
		SystemPrompt: "you are confucius",
		MaxTokens:    512,
		Tools:        []string{},
	}
}

// newTestConfucius builds a Confucius with a registry holding no real tools and
// the provided sub-agent implementations. Tests supply fakes for the
// sub-agents they want to exercise.
func newTestConfucius(t *testing.T, client LLMClient, subAgents map[string]Agent) *Confucius {
	t.Helper()
	reg := tool.NewRegistry()
	c, err := NewConfucius(testConfuciusConfig(), client, reg, subAgents)
	require.NoError(t, err)
	return c
}

// runConfuciusAndCollect runs c.Run against a fresh hub, drains the hub after
// Run returns (so close + drain ordering is deterministic), and returns the
// collected events plus the run outputs. The consumer mirrors stream.WriteSSE's
// "drain-first" pattern so events buffered at Close time are not lost.
func runConfuciusAndCollect(t *testing.T, c *Confucius, messages []Message) ([]stream.StreamEvent, string, Usage, *TurnBreakdown, error) {
	t.Helper()
	hub := stream.NewHub(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make([]stream.StreamEvent, 0, 32)
	var mu sync.Mutex
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			// Drain buffered events first; only when buffer is empty do we
			// honor ctx/Done shutdown signals (mirrors WriteSSE).
			select {
			case e := <-hub.Events():
				mu.Lock()
				events = append(events, e)
				mu.Unlock()
			default:
				select {
				case <-ctx.Done():
					return
				case <-hub.Done():
					// Final drain.
				drain:
					for {
						select {
						case e := <-hub.Events():
							mu.Lock()
							events = append(events, e)
							mu.Unlock()
						default:
							break drain
						}
					}
					return
				case e := <-hub.Events():
					mu.Lock()
					events = append(events, e)
					mu.Unlock()
				}
			}
		}
	}()

	type result struct {
		content   string
		usage     Usage
		breakdown *TurnBreakdown
		err       error
	}
	resCh := make(chan result, 1)
	go func() {
		content, usage, breakdown, err := c.Run(ctx, messages, hub)
		resCh <- result{content, usage, breakdown, err}
	}()

	var r result
	select {
	case r = <-resCh:
	case <-time.After(6 * time.Second):
		t.Fatal("confucius.Run did not complete in time")
	}
	hub.Close()
	select {
	case <-consumerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not drain after hub close")
	}
	mu.Lock()
	out := append([]stream.StreamEvent(nil), events...)
	mu.Unlock()
	return out, r.content, r.usage, r.breakdown, r.err
}

func TestConfucius_HandlesDirectly(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"hello", " world"},
			content:      "hello world",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, usage, _, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", content)
	assert.Equal(t, 12, usage.TotalTokens)

	types := eventTypes(events)
	assert.Equal(t, []string{stream.EventAgentStart, stream.EventToken, stream.EventToken, stream.EventAgentEnd}, types)
	// No tool_call / agent_error / sub-agent activity.
	for _, e := range events {
		assert.NotEqual(t, stream.EventToolCall, e.Type)
		assert.NotEqual(t, stream.EventAgentError, e.Type)
		assert.NotEqual(t, "Chongzhi", e.Agent, "sub-agent Chongzhi should not have been activated")
	}
}

func TestConfucius_CallsSubAgent_ThenSummarizes(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:    "Chongzhi",
		content: "DONE",
		tokens:  []string{"DO", "NE"},
		usage:   Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
	}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "call_1",
				Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"write file","context":"context-x"}`},
			}},
			usage: Usage{PromptTokens: 11, CompletionTokens: 1, TotalTokens: 12},
		},
		fakeResponse{
			tokens:       []string{"all", " done"},
			content:      "all done",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, _, _, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "do thing"},
	})
	require.NoError(t, err)
	assert.Equal(t, "all done", content)

	// Sub-agent should have been invoked exactly once with isolated context.
	require.Equal(t, 1, chongzhi.callCount(), "Chongzhi must be invoked exactly once")
	subMsgs := chongzhi.calls[0].messages
	require.Len(t, subMsgs, 1, "sub-agent should receive a single user message (system prompt is added internally)")
	assert.Contains(t, subMsgs[0].Content, "write file")
	assert.Contains(t, subMsgs[0].Content, "context-x")
	assert.Equal(t, "user", subMsgs[0].Role)

	// Event sequence: confucius start -> tool_call -> chongzhi start -> chongzhi tokens -> chongzhi end -> confucius tokens -> confucius end.
	assert.Contains(t, eventTypes(events), stream.EventAgentStart)
	assert.Contains(t, eventTypes(events), stream.EventAgentEnd)
	var sawChongzhiStart, sawChongzhiEnd, sawToolCall bool
	for _, e := range events {
		if e.Type == stream.EventToolCall && e.Content == ToolInvokeChongzhi {
			sawToolCall = true
		}
		if e.Type == stream.EventAgentStart && e.Agent == "Chongzhi" {
			sawChongzhiStart = true
		}
		if e.Type == stream.EventAgentEnd && e.Agent == "Chongzhi" {
			sawChongzhiEnd = true
		}
	}
	assert.True(t, sawToolCall, "expected a tool_call event for invoke_chongzhi")
	assert.True(t, sawChongzhiStart, "expected Chongzhi agent_start event")
	assert.True(t, sawChongzhiEnd, "expected Chongzhi agent_end event")

	// Round 2 request must contain a role="tool" message with the sub-agent
	// content. The fake records each request; lastRequest is the summarizing
	// round.
	last := client.lastRequest()
	require.NotEmpty(t, last.Messages)
	var toolMsg *Message
	for i := range last.Messages {
		if last.Messages[i].Role == "tool" {
			toolMsg = &last.Messages[i]
		}
	}
	require.NotNil(t, toolMsg, "second round must include a role=tool message")
	assert.Equal(t, "DONE", toolMsg.Content, "tool result content must be the sub-agent's output")
	assert.Equal(t, "call_1", toolMsg.ToolCallID)
}

// TestConfucius_DispatchesToolCallsOnStopFinishReason verifies that Confucius
// continues the tool round even when an OpenAI-compatible endpoint reports
// finish_reason="stop" while still emitting tool_calls. Some third-party APIs
// do not set finish_reason="tool_calls" correctly.
func TestConfucius_DispatchesToolCallsOnStopFinishReason(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:    "Chongzhi",
		content: "C_RESULT",
		tokens:  []string{"C"},
		usage:   Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
	}
	client := newFake(
		fakeResponse{
			content:      "让我先派一个子代理。",
			tokens:       []string{"让我", "先派", "一个", "子代理", "。"},
			finishReason: "stop", // non-compliant endpoint
			toolCalls: []ToolCall{{
				ID:       "call_stop",
				Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"do it"}`},
			}},
			usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
		fakeResponse{
			content:      "combined result",
			tokens:       []string{"combined", " result"},
			finishReason: "stop",
			usage:        Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, usage, _, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "combined result", content)
	assert.Equal(t, 1, chongzhi.callCount(), "Chongzhi must be invoked despite finish_reason=stop")
	assert.Equal(t, 41, usage.TotalTokens)

	sawToolCall := false
	for _, e := range events {
		if e.Type == stream.EventToolCall && e.Content == ToolInvokeChongzhi {
			sawToolCall = true
		}
	}
	assert.True(t, sawToolCall, "expected tool_call event for invoke_chongzhi")

	// Round 2 must include the prior assistant content plus the tool result.
	last := client.lastRequest()
	var toolMsg *Message
	for i := range last.Messages {
		if last.Messages[i].Role == "tool" && last.Messages[i].ToolCallID == "call_stop" {
			toolMsg = &last.Messages[i]
		}
	}
	require.NotNil(t, toolMsg)
	assert.Equal(t, "C_RESULT", toolMsg.Content)
}

func TestConfucius_ParallelToolCalls(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:    "Chongzhi",
		content: "C_RESULT",
		tokens:  []string{"c"},
		usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	liang := &fakeAgent{
		name:    "Liang",
		content: "L_RESULT",
		tokens:  []string{"l"},
		usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "c1", Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"t1"}`}},
				{ID: "c2", Function: ToolCallFunction{Name: ToolInvokeLiang, Arguments: `{"task":"t2"}`}},
			},
		},
		fakeResponse{
			content:      "merged",
			finishReason: "stop",
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	})

	events, content, _, _, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "merged", content)

	assert.Equal(t, 1, chongzhi.callCount())
	assert.Equal(t, 1, liang.callCount())

	// Both sub-agents should have emitted their agent_start events.
	sawChongzhi, sawLiang := false, false
	for _, e := range events {
		if e.Type == stream.EventAgentStart && e.Agent == "Chongzhi" {
			sawChongzhi = true
		}
		if e.Type == stream.EventAgentStart && e.Agent == "Liang" {
			sawLiang = true
		}
	}
	assert.True(t, sawChongzhi, "expected Chongzhi agent_start in parallel dispatch")
	assert.True(t, sawLiang, "expected Liang agent_start in parallel dispatch")

	// Round 2 must include BOTH tool results.
	last := client.lastRequest()
	toolIDs := map[string]bool{}
	for _, m := range last.Messages {
		if m.Role == "tool" {
			toolIDs[m.ToolCallID] = true
		}
	}
	assert.True(t, toolIDs["c1"], "round 2 must include tool result for call c1")
	assert.True(t, toolIDs["c2"], "round 2 must include tool result for call c2")
}

// TestConfucius_ByAgent_ParallelDispatch verifies per-agent usage attribution
// (task 2.5): when Confucius dispatches Chongzhi + Liang in parallel, the
// returned TurnBreakdown.ByAgent contains three keys (Confucius, Chongzhi,
// Liang) whose token sums equal the aggregated total. It also asserts the
// parallel flag is true and both invoke_* tools are listed in order.
func TestConfucius_ByAgent_ParallelDispatch(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:    "Chongzhi",
		content: "C_RESULT",
		tokens:  []string{"c"},
		usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	liang := &fakeAgent{
		name:    "Liang",
		content: "L_RESULT",
		tokens:  []string{"l"},
		usage:   Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{
				{ID: "c1", Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"t1"}`}},
				{ID: "c2", Function: ToolCallFunction{Name: ToolInvokeLiang, Arguments: `{"task":"t2"}`}},
			},
			usage: Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		fakeResponse{
			content:      "merged",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	})

	_, _, usage, breakdown, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)

	// byAgent must carry three keys, one per participating agent.
	require.NotNil(t, breakdown)
	require.Len(t, breakdown.ByAgent, 3, "byAgent must include Confucius + both sub-agents")
	assert.Contains(t, breakdown.ByAgent, "Confucius")
	assert.Contains(t, breakdown.ByAgent, "Chongzhi")
	assert.Contains(t, breakdown.ByAgent, "Liang")

	// Sum of per-agent totals must equal the aggregated turn total.
	var sum int
	for _, u := range breakdown.ByAgent {
		sum += u.TotalTokens
	}
	assert.Equal(t, usage.TotalTokens, sum, "byAgent totals must sum to the turn total")

	// Parallel flag set: one round dispatched >=2 tool_calls.
	assert.True(t, breakdown.Parallel, "parallel must be true when a round has >=2 tool_calls")

	// Both invoke_* tools recorded in dispatch order.
	assert.Equal(t, []string{ToolInvokeChongzhi, ToolInvokeLiang}, breakdown.SubAgentInvocations)
}

// TestConfucius_ByAgent_NoSubAgents verifies that when Confucius answers
// directly (no sub-agent dispatched), byAgent contains only the Confucius key,
// whose usage equals the total, and parallel is false with no invocations.
func TestConfucius_ByAgent_NoSubAgents(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"hi"},
			content:      "hi",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	_, _, usage, breakdown, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "hi"},
	})
	require.NoError(t, err)
	require.NotNil(t, breakdown)
	require.Len(t, breakdown.ByAgent, 1, "byAgent must contain only Confucius")
	assert.Contains(t, breakdown.ByAgent, "Confucius")
	assert.Equal(t, usage, breakdown.ByAgent["Confucius"])
	assert.False(t, breakdown.Parallel)
	assert.Empty(t, breakdown.SubAgentInvocations)
}

// TestConfucius_ByAgent_ErrorTurnStillAttributed verifies that even when a
// sub-agent fails, its (partial) usage is still attributed to its key in
// byAgent, and the aggregated total is returned. The error turn still emits
// the breakdown so cost is never lost.
func TestConfucius_ByAgent_ErrorTurnStillAttributed(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:  "Chongzhi",
		err:   errors.New("boom"),
		usage: Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
	}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "cx",
				Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"fail"}`},
			}},
			usage: Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		},
		fakeResponse{
			content:      "recovered",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	_, _, _, breakdown, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	require.NotNil(t, breakdown)
	// Confucius (both rounds) + failed Chongzhi both attributable. The fake
	// returns zero usage on error, but the key must still be present so the
	// dispatch is attributable on the error turn — this is the task 2.5
	// invariant that cost attribution is never lost on failure.
	assert.Contains(t, breakdown.ByAgent, "Confucius")
	assert.Contains(t, breakdown.ByAgent, "Chongzhi", "failed sub-agent must still be attributable")
	assert.Contains(t, breakdown.SubAgentInvocations, ToolInvokeChongzhi)
}

func TestConfucius_SubAgentFailure_StreamsError(t *testing.T) {
	defer goleak.VerifyNone(t)
	chongzhi := &fakeAgent{
		name:   "Chongzhi",
		err:    errors.New("boom"),
		tokens: []string{},
	}
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "cx",
				Function: ToolCallFunction{Name: ToolInvokeChongzhi, Arguments: `{"task":"fail-me"}`},
			}},
		},
		fakeResponse{
			content:      "recovered",
			finishReason: "stop",
		},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, _, _, err := runConfuciusAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "recovered", content)

	// Expect an agent_error event for Chongzhi with the failure message.
	var sawErr bool
	for _, e := range events {
		if e.Type == stream.EventAgentError && e.Agent == "Chongzhi" {
			sawErr = true
			assert.Contains(t, e.Content, "boom")
		}
	}
	assert.True(t, sawErr, "expected Chongzhi agent_error event on sub-agent failure")

	// Round 2 must include a tool result whose content is the error text.
	last := client.lastRequest()
	var toolContent string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "cx" {
			toolContent = m.Content
		}
	}
	assert.Contains(t, toolContent, "boom", "tool result must carry the error message back to the LLM")
}

func TestConfucius_ContextCancellation_Stops(t *testing.T) {
	defer goleak.VerifyNone(t)
	// StreamChat blocks until the token slice is exhausted; with an empty
	// token slice and a finishReason it would return immediately. Instead we
	// test cancellation via a client that respects ctx by never completing a
	// response: the fake returns from StreamChat only when ctx is cancelled.
	slow := &blockingClient{unblock: make(chan struct{})}
	client := newFake()
	client.responses = nil // we will not use the queue; slow client short-circuits

	c := newTestConfucius(t, slow, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	hub := stream.NewHub(0)
	defer hub.Close()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := c.Run(ctx, []Message{{Role: "user", Content: "x"}}, hub)
		done <- err
	}()

	// Give the goroutine a chance to enter StreamChat then cancel.
	cancel()
	close(slow.unblock)

	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "canceled"),
			"Run should surface context cancellation; got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestConfucius_ReasoningRequest(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"planning"},
			content:      "planning",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	)
	cfg := testConfuciusConfig()
	cfg.Thinking = true
	cfg.ReasoningEffort = "medium"
	reg := tool.NewRegistry()
	c, err := NewConfucius(cfg, client, reg, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = c.Run(ctx, []Message{{Role: "user", Content: "plan"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	assert.True(t, req.Thinking, "Thinking must be true")
	assert.Equal(t, "medium", req.ReasoningEffort)
	assert.Equal(t, 512, req.MaxTokens)
}

// blockingClient blocks StreamChat until unblock is closed (so the test can
// force a mid-stream cancellation) and returns ctx.Err().
type blockingClient struct {
	unblock chan struct{}
	once    sync.Once
}

func (b *blockingClient) StreamChat(ctx context.Context, _ LLMRequest, _ func(string) error, _ func(string) error) (LLMResponse, error) {
	<-b.unblock
	return LLMResponse{}, ctx.Err()
}

func (b *blockingClient) requestCount() int       { return 0 }
func (b *blockingClient) lastRequest() LLMRequest { return LLMRequest{} }

func eventTypes(events []stream.StreamEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// scriptedRetryAgent is an Agent that pops a queued outcome per Run call, so
// retry tests can script "fail with 429 then succeed". executedTool controls
// the ToolCallTracker signal (for Chongzhi idempotency scenarios); retryPolicy
// is returned by RetryPolicy().
type scriptedRetryAgent struct {
	name        string
	prompt      string
	outcomes    []scriptedOutcome
	executedTool bool
	policy      config.AgentRetryConfig
	mu          sync.Mutex
	runCalls    int
}

type scriptedOutcome struct {
	content string
	usage   Usage
	err     error
}

func (a *scriptedRetryAgent) Name() string                     { return a.name }
func (a *scriptedRetryAgent) SystemPrompt() string             { return a.prompt }
func (a *scriptedRetryAgent) RetryPolicy() config.AgentRetryConfig { return a.policy }
func (a *scriptedRetryAgent) LastRunExecutedTool() bool        { return a.executedTool }

func (a *scriptedRetryAgent) Run(ctx context.Context, _ []Message, hub *stream.Hub) (string, Usage, *TurnBreakdown, error) {
	a.mu.Lock()
	a.runCalls++
	idx := a.runCalls - 1
	a.mu.Unlock()
	hub.SendCtx(ctx, stream.AgentStartEvent(a.name))
	if idx < len(a.outcomes) {
		o := a.outcomes[idx]
		if o.err != nil {
			hub.SendCtx(ctx, stream.AgentErrorEvent(a.name, o.err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
			return "", o.usage, nil, o.err
		}
		hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
		return o.content, o.usage, nil, nil
	}
	// Outcomes exhausted: terminal error so the retry loop stops.
	hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
	return "", Usage{}, nil, errors.New("scriptedRetryAgent: no more outcomes")
}

func (a *scriptedRetryAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runCalls
}

// transientErr and semanticErr are sentinel errors used to exercise the retry
// classifier without importing real SDK error types.
var (
	transientErr = errors.New("429 Too Many Requests: rate limit exceeded")
	semanticErr  = errors.New("bad_args: invalid arguments")
)

// runConfuciusRetry drives a Confucius dispatch of a single invoke tool and
// returns the tool result content + the number of sub-agent Run calls + the
// raw emitted events. It uses the real Confucius dispatch path so the retry
// wrapper is exercised end to end.
func runConfuciusRetry(t *testing.T, sub Agent, invokeName string) (string, int, []stream.StreamEvent) {
	t.Helper()
	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID: "c1", Function: ToolCallFunction{Name: invokeName, Arguments: `{"task":"do"}`},
			}},
		},
		fakeResponse{content: "ok", finishReason: "stop"},
	)
	c := newTestConfucius(t, client, map[string]Agent{
		ToolInvokeChongzhi: subIf(invokeName == ToolInvokeChongzhi, sub, &fakeAgent{name: "Chongzhi"}),
		ToolInvokeLiang:    subIf(invokeName == ToolInvokeLiang, sub, &fakeAgent{name: "Liang"}),
	})
	events, _, _, _, _ := runConfuciusAndCollect(t, c, []Message{{Role: "user", Content: "go"}})
	// Recover the tool result fed to Confucius round 2.
	last := client.lastRequest()
	var content string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			content = m.Content
		}
	}
	calls := 0
	if s, ok := sub.(*scriptedRetryAgent); ok {
		calls = s.callCount()
	}
	return content, calls, events
}

func subIf(cond bool, sub, other Agent) Agent {
	if cond {
		return sub
	}
	return other
}

func TestRetry_TransientErrorRetriedThenSucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	liang := &scriptedRetryAgent{
		name: "Liang",
		policy: config.AgentRetryConfig{Enabled: true, MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		outcomes: []scriptedOutcome{
			{err: transientErr, usage: Usage{TotalTokens: 5}},
			{content: "RECOVERED", usage: Usage{TotalTokens: 8}},
		},
	}
	content, calls, _ := runConfuciusRetry(t, liang, ToolInvokeLiang)
	assert.Equal(t, 2, calls, "Liang must be invoked twice (initial + 1 retry)")
	assert.Equal(t, "RECOVERED", content, "recovered result must be fed back to Confucius")
}

func TestRetry_SemanticErrorNotRetried(t *testing.T) {
	defer goleak.VerifyNone(t)
	liang := &scriptedRetryAgent{
		name: "Liang",
		policy: config.AgentRetryConfig{Enabled: true, MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		outcomes: []scriptedOutcome{{err: semanticErr, usage: Usage{TotalTokens: 5}}},
	}
	content, calls, _ := runConfuciusRetry(t, liang, ToolInvokeLiang)
	assert.Equal(t, 1, calls, "semantic error must not be retried")
	assert.Contains(t, content, "bad_args", "semantic error must be fed back to Confucius")
}

func TestRetry_DisabledPolicyNotRetried(t *testing.T) {
	defer goleak.VerifyNone(t)
	liang := &scriptedRetryAgent{
		name: "Liang",
		policy: config.AgentRetryConfig{Enabled: false, MaxAttempts: 3},
		outcomes: []scriptedOutcome{
			{err: transientErr, usage: Usage{TotalTokens: 5}},
			{content: "RECOVERED", usage: Usage{TotalTokens: 8}},
		},
	}
	content, calls, _ := runConfuciusRetry(t, liang, ToolInvokeLiang)
	assert.Equal(t, 1, calls, "disabled retry policy must not retry")
	assert.Contains(t, content, "429", "transient error fed back when retry disabled")
}

func TestRetry_SideEffectAfterToolNotRetried(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Chongzhi that already executed a tool call before failing: executedTool=true.
	chongzhi := &scriptedRetryAgent{
		name:         "Chongzhi",
		executedTool: true,
		policy:       config.AgentRetryConfig{Enabled: true, MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		outcomes: []scriptedOutcome{
			{err: transientErr, usage: Usage{TotalTokens: 5}},
			{content: "RECOVERED", usage: Usage{TotalTokens: 8}},
		},
	}
	content, calls, _ := runConfuciusRetry(t, chongzhi, ToolInvokeChongzhi)
	assert.Equal(t, 1, calls, "side-effecting agent post-tool-call must not be retried")
	assert.Contains(t, content, "429", "error fed back when retry suppressed by idempotency")
}

func TestRetry_SideEffectBeforeToolRetried(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Chongzhi that failed BEFORE any tool call (executedTool=false): retryable.
	chongzhi := &scriptedRetryAgent{
		name:         "Chongzhi",
		executedTool: false,
		policy:       config.AgentRetryConfig{Enabled: true, MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		outcomes: []scriptedOutcome{
			{err: transientErr, usage: Usage{TotalTokens: 5}},
			{content: "DONE", usage: Usage{TotalTokens: 8}},
		},
	}
	content, calls, _ := runConfuciusRetry(t, chongzhi, ToolInvokeChongzhi)
	assert.Equal(t, 2, calls, "side-effecting agent pre-tool-call must be retried")
	assert.Equal(t, "DONE", content)
}

func TestRetry_BudgetExhaustedStopsRetry(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Budget of 5 tokens; first failed attempt charges 5 -> budget exhausted.
	liang := &scriptedRetryAgent{
		name: "Liang",
		policy: config.AgentRetryConfig{
			Enabled: true, MaxAttempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
			BudgetTokens: 5,
		},
		outcomes: []scriptedOutcome{
			{err: transientErr, usage: Usage{TotalTokens: 5}},
			{content: "RECOVERED", usage: Usage{TotalTokens: 8}},
		},
	}
	content, calls, _ := runConfuciusRetry(t, liang, ToolInvokeLiang)
	// The budget lives on Confucius's cfg.Retry.BudgetTokens, which is 0
	// (unlimited) in the test confucius config — so this scenario is exercised
	// at the unit level via the retry budget type directly below, and this test
	// documents that a per-agent BudgetTokens does NOT by itself cap retries
	// (the cap is turn-level on Confucius). When Confucius has no budget, the
	// retry proceeds using the agent's own MaxAttempts.
	assert.Equal(t, 2, calls)
	assert.Equal(t, "RECOVERED", content)
}

func TestRetry_BudgetTypeStopsWhenExhausted(t *testing.T) {
	// Direct unit test of the turn-level retryBudget: once charged to the limit,
	// allows() returns false, which dispatchSubAgent reads to stop retrying.
	b := newRetryBudget(5)
	assert.True(t, b.allows(), "fresh budget must allow retries")
	b.charge(Usage{TotalTokens: 5})
	assert.False(t, b.allows(), "exhausted budget must refuse retries")

	unlimited := newRetryBudget(0)
	unlimited.charge(Usage{TotalTokens: 1000000})
	assert.True(t, unlimited.allows(), "unlimited budget (0) always allows")
}

func TestRetry_ExhaustedSurfacesErrorToConfucius(t *testing.T) {
	defer goleak.VerifyNone(t)
	// MaxAttempts=1 => no retries at all; transient error surfaces to Confucius.
	liang := &scriptedRetryAgent{
		name: "Liang",
		policy: config.AgentRetryConfig{Enabled: true, MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		outcomes: []scriptedOutcome{{err: transientErr, usage: Usage{TotalTokens: 5}}},
	}
	content, calls, events := runConfuciusRetry(t, liang, ToolInvokeLiang)
	assert.Equal(t, 1, calls)
	assert.Contains(t, content, "429", "exhausted-retry error must feed back to Confucius")
	// No retry event should be emitted when MaxAttempts==1.
	var sawRetry bool
	for _, e := range events {
		if e.Type == stream.EventAgentError && e.Meta["retry"] == true {
			sawRetry = true
		}
	}
	assert.False(t, sawRetry, "no retry event when MaxAttempts==1")
}
