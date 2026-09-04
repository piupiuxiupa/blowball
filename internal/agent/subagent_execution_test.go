package agent

import (
	"context"
	"encoding/json"
	"errors"
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

func testExecutionConfig(tools ...string) config.AgentConfig {
	return config.AgentConfig{
		Name:         "Execution",
		SystemPrompt: "you are chongzhi",
		Tools:        tools,
	}
}

// fakeExecutor records invocations of a tool registered under a chosen name.
// The returned result/error is configurable so tests can drive both happy and
// failure paths.
type fakeExecutor struct {
	name   string
	result any
	err    error
	mu     struct {
		sync.Mutex
		calls []json.RawMessage
	}
}

func (f *fakeExecutor) execute(_ context.Context, args json.RawMessage) (any, error) {
	f.mu.Lock()
	f.mu.calls = append(f.mu.calls, args)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mu.calls)
}

func newTestSubAgent(t *testing.T, client LLMClient, reg *tool.Registry) *SubAgent {
	t.Helper()
	// Chongzhi config names exactly the tools present in reg. The caller
	// builds reg with the fake tool it wants to exercise.
	names := []string{}
	for _, s := range reg.List() {
		names = append(names, s.Name)
	}
	c, err := newGenericFromAgentConfig(testExecutionConfig(names...), client, reg, testTurn())
	require.NoError(t, err)
	return c
}

func newSubAgentRegistryWithFake(t *testing.T, fake *fakeExecutor) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	spec := &tool.ToolSpec{
		Name:           fake.name,
		Description:    "fake tool",
		ParametersJSON: json.RawMessage(`{"type":"object"}`),
		Execute:        fake.execute,
	}
	require.NoError(t, reg.Register(spec))
	return reg
}

// runSubAgentAndCollect mirrors runConfuciusAndCollect for Chongzhi.
func runSubAgentAndCollect(t *testing.T, c *SubAgent, messages []Message) ([]stream.StreamEvent, string, Usage, error) {
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
		content, usage, _, err := c.Run(ctx, messages, hub)
		resCh <- result{content, usage, err}
	}()

	var r result
	select {
	case r = <-resCh:
	case <-time.After(6 * time.Second):
		t.Fatal("chongzhi.Run did not complete in time")
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

func TestSubAgentExecution_RunsXizhiTool(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_write_file", result: map[string]any{"ok": true}}
	reg := newSubAgentRegistryWithFake(t, fake)

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "t1",
				Function: ToolCallFunction{Name: "xizhi_write_file", Arguments: `{"path":"a.txt","content":"hi"}`},
			}},
		},
		fakeResponse{
			content:      "wrote the file",
			finishReason: "stop",
			tokens:       []string{"wrote", " file"},
		},
	)
	c := newTestSubAgent(t, client, reg)

	events, content, _, err := runSubAgentAndCollect(t, c, []Message{
		{Role: "user", Content: "write a.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, "wrote the file", content)

	// The tool must have been invoked once with the parsed args.
	require.Equal(t, 1, fake.callCount(), "xizhi_write_file must be invoked once")
	var args map[string]any
	require.NoError(t, json.Unmarshal(fake.mu.calls[0], &args))
	assert.Equal(t, "a.txt", args["path"])

	// Round 2 must include the tool result fed back.
	last := client.lastRequest()
	var toolContent string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			toolContent = m.Content
		}
	}
	assert.Contains(t, toolContent, `"ok":true`, "tool result must be JSON-marshaled into the tool message")

	// Confirm tool_call event was streamed.
	sawTool := false
	for _, e := range events {
		if e.Type == stream.EventToolCall && e.Content == "xizhi_write_file" {
			sawTool = true
		}
	}
	assert.True(t, sawTool, "expected tool_call event for xizhi_write_file")
}

// TestSubAgentExecution_ToolFailure_NoAgentError verifies that a registry-tool failure
// is carried solely by the status envelope in the role="tool" message and does
// NOT emit an agent_error SSE event. The frontend renders tool errors from the
// status field; surfacing them again as agent_error misrepresents a recoverable
// tool hiccup as an agent failure (capability: tool-result-envelope). This guards
// the shared leaf-agent dispatchOneRegistryTool path used by Chongzhi/Liang and
// the parallel structure of Confucius's dispatchRegistryTool.
func TestSubAgentExecution_ToolFailure_NoAgentError(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_write_file", err: errors.New("disk full")}
	reg := newSubAgentRegistryWithFake(t, fake)

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "t1",
				Function: ToolCallFunction{Name: "xizhi_write_file", Arguments: `{"path":"a.txt","content":"hi"}`},
			}},
		},
		fakeResponse{
			content:      "could not write the file",
			finishReason: "stop",
			tokens:       []string{"could not write the file"},
		},
	)
	c := newTestSubAgent(t, client, reg)

	events, _, _, err := runSubAgentAndCollect(t, c, []Message{
		{Role: "user", Content: "write a.txt"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, fake.callCount(), "xizhi_write_file must be invoked once (failure path)")

	// The model must receive the failure in-band via the status envelope.
	last := client.lastRequest()
	var toolContent string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			toolContent = m.Content
		}
	}
	assert.Contains(t, toolContent, `"status":1`, "tool result envelope must mark failure")
	assert.Contains(t, toolContent, `"error":"disk full"`, "tool result envelope must carry the error message")

	// No agent_error event must be emitted for a registry-tool failure.
	for _, e := range events {
		assert.NotEqual(t, stream.EventAgentError, e.Type, "a registry-tool failure must not emit an agent_error event")
	}
}

// TestSubAgentExecution_DispatchesToolCallsOnStopFinishReason verifies that Chongzhi
// dispatches tool_calls even when the finish_reason is "stop" rather than the
// native "tool_calls" value.
func TestSubAgentExecution_DispatchesToolCallsOnStopFinishReason(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_write_file", result: map[string]any{"ok": true}}
	reg := newSubAgentRegistryWithFake(t, fake)

	client := newFake(
		fakeResponse{
			content:      "让我先写文件。",
			tokens:       []string{"让我", "先写", "文件", "。"},
			finishReason: "stop", // non-compliant endpoint
			toolCalls: []ToolCall{{
				ID:       "t1",
				Function: ToolCallFunction{Name: "xizhi_write_file", Arguments: `{"path":"a.txt","content":"hi"}`},
			}},
		},
		fakeResponse{
			content:      "wrote the file",
			finishReason: "stop",
			tokens:       []string{"wrote", " file"},
		},
	)
	c := newTestSubAgent(t, client, reg)

	events, content, _, err := runSubAgentAndCollect(t, c, []Message{
		{Role: "user", Content: "write a.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, "wrote the file", content)
	require.Equal(t, 1, fake.callCount(), "xizhi_write_file must be invoked despite finish_reason=stop")

	sawTool := false
	for _, e := range events {
		if e.Type == stream.EventToolCall && e.Content == "xizhi_write_file" {
			sawTool = true
		}
	}
	assert.True(t, sawTool, "expected tool_call event for xizhi_write_file")

	// Round 2 must include the tool result fed back.
	last := client.lastRequest()
	var toolContent string
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			toolContent = m.Content
		}
	}
	assert.Contains(t, toolContent, `"ok":true`, "tool result must be JSON-marshaled into the tool message")
}

func TestSubAgentExecution_FlatTopology_NoInvokeTools(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_read_file", result: ""}
	reg := newSubAgentRegistryWithFake(t, fake)

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "t-inv",
				Function: ToolCallFunction{Name: SpawnSubagentTool, Arguments: `{"task":"recurse"}`},
			}},
		},
		fakeResponse{
			content:      "fallback",
			finishReason: "stop",
		},
	)
	c := newTestSubAgent(t, client, reg)

	_, content, _, err := runSubAgentAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fallback", content)

	// Default-depth contract: spawn_subagent is not advertised to a child.
	assert.NotContains(t, string(c.toolsJSON), SpawnSubagentTool,
		"a depth-capped generic sub-agent must not receive the spawn tool")

	// The fake xizhi tool should not have been touched.
	assert.Equal(t, 0, fake.callCount(), "xizhi tool must not be invoked by an invoke_liang tool_call")
}

func TestSubAgentExecution_ReasoningRequest(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"thinking", "..."},
			content:      "thinking...",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		},
	)
	reg := tool.NewRegistry()
	cfg := testExecutionConfig()
	// The thinking wire family and output quota both ride the turn config
	// (model-effort-v2, per-model-completion-budget), not the agent config.
	turn := testTurn()
	turn.Thinking, turn.ReasoningEffort, turn.MaxCompletionTokens = true, "high", 1024
	c, err := newGenericFromAgentConfig(cfg, client, reg, turn)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = c.Run(ctx, []Message{{Role: "user", Content: "hello"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	assert.True(t, req.Thinking, "Thinking must be true")
	assert.Equal(t, "high", req.ReasoningEffort)
	assert.Equal(t, 1024, req.MaxCompletionTokens)
}
