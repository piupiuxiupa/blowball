package agent

import (
	"context"
	"encoding/json"
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

func testChongzhiConfig(tools ...string) config.AgentConfig {
	return config.AgentConfig{
		Name:         "Chongzhi",
		Model:        "gpt-test",
		SystemPrompt: "you are chongzhi",
		MaxTokens:    1024,
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

func newTestChongzhi(t *testing.T, client LLMClient, reg *tool.Registry) *Chongzhi {
	t.Helper()
	// Chongzhi config names exactly the tools present in reg. The caller
	// builds reg with the fake tool it wants to exercise.
	names := []string{}
	for _, s := range reg.List() {
		names = append(names, s.Name)
	}
	c, err := NewChongzhi(testChongzhiConfig(names...), client, reg)
	require.NoError(t, err)
	return c
}

func newChongzhiRegistryWithFake(t *testing.T, fake *fakeExecutor) *tool.Registry {
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

// runChongzhiAndCollect mirrors runConfuseAndCollect for Chongzhi.
func runChongzhiAndCollect(t *testing.T, c *Chongzhi, messages []Message) ([]stream.StreamEvent, string, Usage, error) {
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
		content, usage, err := c.Run(ctx, messages, hub)
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

func TestChongzhi_RunsXizhiTool(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_write_file", result: map[string]any{"ok": true}}
	reg := newChongzhiRegistryWithFake(t, fake)

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
	c := newTestChongzhi(t, client, reg)

	events, content, _, err := runChongzhiAndCollect(t, c, []Message{
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

func TestChongzhi_FlatTopology_NoInvokeTools(t *testing.T) {
	defer goleak.VerifyNone(t)
	fake := &fakeExecutor{name: "xizhi_read_file", result: ""}
	reg := newChongzhiRegistryWithFake(t, fake)

	client := newFake(
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID:       "t-inv",
				Function: ToolCallFunction{Name: ToolInvokeLiang, Arguments: `{"task":"recurse"}`},
			}},
		},
		fakeResponse{
			content:      "fallback",
			finishReason: "stop",
		},
	)
	c := newTestChongzhi(t, client, reg)

	events, content, _, err := runChongzhiAndCollect(t, c, []Message{
		{Role: "user", Content: "go"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fallback", content)

	// Flat-topology contract: invoke_* tools are NEVER recognized by Chongzhi.
	// The tool_call must error as "unknown tool" (registry miss) rather than
	// dispatching the sub-agent.
	var sawUnknownErr bool
	for _, e := range events {
		if e.Type == stream.EventAgentError && e.Agent == "Chongzhi" {
			if strings.Contains(e.Content, "unknown tool") || strings.Contains(e.Content, ToolInvokeLiang) {
				sawUnknownErr = true
			}
		}
	}
	assert.True(t, sawUnknownErr, "Chongzhi must surface an unknown-tool error for invoke_liang; flat topology forbids sub-agent recursion")

	// The fake xizhi tool should not have been touched.
	assert.Equal(t, 0, fake.callCount(), "xizhi tool must not be invoked by an invoke_liang tool_call")
}
