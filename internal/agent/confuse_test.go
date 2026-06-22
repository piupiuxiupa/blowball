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

func testConfuseConfig() config.AgentConfig {
	return config.AgentConfig{
		Name:         "Confuse",
		Model:        "gpt-test",
		SystemPrompt: "you are confuse",
		MaxTokens:    512,
		Tools:        []string{},
	}
}

// newTestConfuse builds a Confuse with a registry holding no real tools and
// the provided sub-agent implementations. Tests supply fakes for the
// sub-agents they want to exercise.
func newTestConfuse(t *testing.T, client LLMClient, subAgents map[string]Agent) *Confuse {
	t.Helper()
	reg := tool.NewRegistry()
	c, err := NewConfuse(testConfuseConfig(), client, reg, subAgents)
	require.NoError(t, err)
	return c
}

// runConfuseAndCollect runs c.Run against a fresh hub, drains the hub after
// Run returns (so close + drain ordering is deterministic), and returns the
// collected events plus the run outputs. The consumer mirrors stream.WriteSSE's
// "drain-first" pattern so events buffered at Close time are not lost.
func runConfuseAndCollect(t *testing.T, c *Confuse, messages []Message) ([]stream.StreamEvent, string, Usage, error) {
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
		content string
		usage   Usage
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		content, usage, err := c.Run(ctx, messages, hub)
		resCh <- result{content, usage, err}
	}()

	var r result
	select {
	case r = <-resCh:
	case <-time.After(6 * time.Second):
		t.Fatal("confuse.Run did not complete in time")
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
	return out, r.content, r.usage, r.err
}

func TestConfuse_HandlesDirectly(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"hello", " world"},
			content:      "hello world",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
	)
	c := newTestConfuse(t, client, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, usage, err := runConfuseAndCollect(t, c, []Message{
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

func TestConfuse_CallsSubAgent_ThenSummarizes(t *testing.T) {
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
	c := newTestConfuse(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, _, err := runConfuseAndCollect(t, c, []Message{
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

	// Event sequence: confuse start -> tool_call -> chongzhi start -> chongzhi tokens -> chongzhi end -> confuse tokens -> confuse end.
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

func TestConfuse_ParallelToolCalls(t *testing.T) {
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
	c := newTestConfuse(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	})

	events, content, _, err := runConfuseAndCollect(t, c, []Message{
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

func TestConfuse_SubAgentFailure_StreamsError(t *testing.T) {
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
	c := newTestConfuse(t, client, map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	events, content, _, err := runConfuseAndCollect(t, c, []Message{
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

func TestConfuse_ContextCancellation_Stops(t *testing.T) {
	defer goleak.VerifyNone(t)
	// StreamChat blocks until the token slice is exhausted; with an empty
	// token slice and a finishReason it would return immediately. Instead we
	// test cancellation via a client that respects ctx by never completing a
	// response: the fake returns from StreamChat only when ctx is cancelled.
	slow := &blockingClient{unblock: make(chan struct{})}
	client := newFake()
	client.responses = nil // we will not use the queue; slow client short-circuits

	c := newTestConfuse(t, slow, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	hub := stream.NewHub(0)
	defer hub.Close()

	done := make(chan error, 1)
	go func() {
		_, _, err := c.Run(ctx, []Message{{Role: "user", Content: "x"}}, hub)
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

func TestConfuse_ReasoningRequest(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"planning"},
			content:      "planning",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	)
	cfg := testConfuseConfig()
	cfg.Thinking = true
	cfg.ReasoningEffort = "medium"
	reg := tool.NewRegistry()
	c, err := NewConfuse(cfg, client, reg, map[string]Agent{
		ToolInvokeChongzhi: &fakeAgent{name: "Chongzhi"},
		ToolInvokeLiang:    &fakeAgent{name: "Liang"},
	})
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = c.Run(ctx, []Message{{Role: "user", Content: "plan"}}, hub)
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
