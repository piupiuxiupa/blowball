package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestLiang_NoTools_PassesEmptyToolsJSON(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"answer"},
			content:      "answer",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	)
	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
		// Tools intentionally empty.
	}, client, tool.NewRegistry())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, _, _, _ = liang.Run(ctx, []Message{{Role: "user", Content: "hello"}}, hub)
	}()

	// Drain until first token observed or timeout.
	var gotToken bool
drain:
	for {
		select {
		case e := <-hub.Events():
			if e.Type == stream.EventToken {
				gotToken = true
			}
		case <-ctx.Done():
			break drain
		case <-hub.Done():
			break drain
		}
		if gotToken {
			break drain
		}
	}
	hub.Close()

	require.True(t, gotToken, "Liang must stream at least one token")
	// Tools field must be nil / empty: agent must not even send "[]".
	last := client.lastRequest()
	assert.Nil(t, last.Tools, "Liang's LLMRequest.Tools must be nil; got %v", last.Tools)
}

func TestLiang_ToolCall(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(
		&tool.ToolSpec{
			Name:           "ping",
			Description:    "reply with pong",
			ParametersJSON: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				return "pong", nil
			},
		},
	))

	client := newFake(
		fakeResponse{
			tokens:       []string{"call"},
			content:      "",
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{
				ID: "tc_1",
				Function: ToolCallFunction{
					Name:      "ping",
					Arguments: `{"msg":"hello"}`,
				},
			}},
			usage: Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		},
		fakeResponse{
			tokens:       []string{"done"},
			content:      "done",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
		},
	)

	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
		Tools:        []string{"ping"},
	}, client, reg)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	content, usage, _, err := liang.Run(ctx, []Message{{Role: "user", Content: "ping"}}, hub)
	require.NoError(t, err)
	require.Equal(t, "done", content)
	require.Equal(t, 16, usage.TotalTokens)
	hub.Close()

	// Verify the tool call was included in the final LLM request's messages.
	require.Equal(t, 2, client.requestCount())
	last := client.lastRequest()
	require.Len(t, last.Messages, 4)
	assert.Equal(t, "tool", last.Messages[3].Role)
	assert.Equal(t, "tc_1", last.Messages[3].ToolCallID)
	assert.Contains(t, last.Messages[3].Content, "pong")
}

// TestLiang_DispatchesToolCallsOnStopFinishReason verifies that Liang dispatches
// tool_calls even when the finish_reason is "stop" rather than the native
// "tool_calls" value.
func TestLiang_DispatchesToolCallsOnStopFinishReason(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(
		&tool.ToolSpec{
			Name:           "ping",
			Description:    "reply with pong",
			ParametersJSON: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				return "pong", nil
			},
		},
	))

	client := newFake(
		fakeResponse{
			content:      "我调用一下工具。",
			tokens:       []string{"我", "调用", "一下", "工具", "。"},
			finishReason: "stop", // non-compliant endpoint
			toolCalls: []ToolCall{{
				ID: "tc_stop",
				Function: ToolCallFunction{
					Name:      "ping",
					Arguments: `{"msg":"hello"}`,
				},
			}},
			usage: Usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10},
		},
		fakeResponse{
			tokens:       []string{"done"},
			content:      "done",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
		},
	)

	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
		Tools:        []string{"ping"},
	}, client, reg)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	content, usage, _, err := liang.Run(ctx, []Message{{Role: "user", Content: "ping"}}, hub)
	require.NoError(t, err)
	require.Equal(t, "done", content)
	require.Equal(t, 19, usage.TotalTokens)
	hub.Close()

	require.Equal(t, 2, client.requestCount())
	last := client.lastRequest()
	require.Len(t, last.Messages, 4)
	assert.Equal(t, "tool", last.Messages[3].Role)
	assert.Equal(t, "tc_stop", last.Messages[3].ToolCallID)
	assert.Contains(t, last.Messages[3].Content, "pong")
}

func TestLiang_StreamsTokens(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			// Token deltas already include the spaces so streamed concatenation
			// matches the final content exactly.
			tokens:       []string{"one", " ", "two", " ", "three"},
			content:      "one two three",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
		},
	)
	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
	}, client, tool.NewRegistry())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type res struct {
		content string
		usage   Usage
		err     error
	}
	resCh := make(chan res, 1)
	go func() {
		c, u, _, e := liang.Run(ctx, []Message{{Role: "user", Content: "count"}}, hub)
		resCh <- res{c, u, e}
	}()

	var events []stream.StreamEvent
consumer:
	for {
		select {
		case e := <-hub.Events():
			events = append(events, e)
		case r := <-resCh:
			require.NoError(t, r.err)
			assert.Equal(t, "one two three", r.content)
			assert.Equal(t, 5, r.usage.TotalTokens)
			// Final drain.
		drain:
			for {
				select {
				case e := <-hub.Events():
					events = append(events, e)
				default:
					break drain
				}
			}
			break consumer
		case <-time.After(2 * time.Second):
			t.Fatal("Liang.Run did not complete")
		}
	}
	hub.Close()

	// Verify lifecycle: agent_start, 3 tokens, agent_end.
	types := eventTypes(events)
	assert.Equal(t, stream.EventAgentStart, types[0], "first event must be agent_start")
	assert.Equal(t, stream.EventAgentEnd, types[len(types)-1], "last event must be agent_end")

	tokenCount := 0
	for _, e := range events {
		if e.Type == stream.EventToken {
			tokenCount++
		}
	}
	assert.Equal(t, 5, tokenCount, "expected exactly 5 token events")
}

func TestLiang_ReasoningRequest(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"analyzing"},
			content:      "analyzing",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		},
	)
	liang, err := NewLiang(config.AgentConfig{
		Name:            "Liang",
		Model:           "gpt-test",
		SystemPrompt:    "you are liang",
		MaxTokens:       512,
		Thinking:        true,
		ReasoningEffort: "low",
	}, client, tool.NewRegistry())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	assert.True(t, req.Thinking, "Thinking must be true")
	assert.Equal(t, "low", req.ReasoningEffort)
	assert.Equal(t, 512, req.MaxTokens)
}

// TestLiang_OutputSchema_NoTools_SetsResponseFormatOnTerminalRound verifies
// that a Liang configured with output_schema (and no tools) attaches
// response_format: json_schema to its single (terminal) round so the content
// returned to Confucius conforms to the schema (capability A).
func TestLiang_OutputSchema_NoTools_SetsResponseFormatOnTerminalRound(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"{\"verdict\":\"ok\"}"},
			content:      `{"verdict":"ok"}`,
			finishReason: "stop",
			usage:        Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		},
	)
	schema := `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`
	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
		OutputSchema: schema,
	}, client, tool.NewRegistry())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	require.NotEmpty(t, req.ResponseFormat, "terminal round must carry response_format when output_schema is set")
	var rf map[string]any
	require.NoError(t, json.Unmarshal(req.ResponseFormat, &rf))
	assert.Equal(t, "json_schema", rf["type"])
	js, ok := rf["json_schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Liang", js["name"])
	assert.Equal(t, true, js["strict"])
}

// TestLiang_OutputSchema_ToolCall_TerminalRoundOnly verifies that a tooled
// Liang with output_schema attaches response_format ONLY on the terminal
// (post-tool-dispatch) round, NOT on the intermediate tool-call round.
func TestLiang_OutputSchema_ToolCall_TerminalRoundOnly(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg := tool.NewRegistry()
	require.NoError(t, reg.Register(
		&tool.ToolSpec{
			Name:           "ping",
			Description:    "reply with pong",
			ParametersJSON: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
			Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
				return "pong", nil
			},
		},
	))
	client := newFake(
		// Round 1 (intermediate): emits a tool_call -> must NOT carry response_format.
		fakeResponse{
			finishReason: "tool_calls",
			toolCalls: []ToolCall{{ID: "tc_1", Function: ToolCallFunction{Name: "ping", Arguments: `{"msg":"x"}`}}},
			usage: Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		},
		// Round 2 (terminal): stop -> MUST carry response_format.
		fakeResponse{
			content:      `{"verdict":"ok"}`,
			finishReason: "stop",
			usage:        Usage{PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
		},
	)
	schema := `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}`
	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
		Tools:        []string{"ping"},
		OutputSchema: schema,
	}, client, reg)
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	require.NoError(t, err)
	hub.Close()

	require.Equal(t, 2, client.requestCount(), "expected exactly two LLM rounds")
	reqs := client.calls
	// Round 1 must NOT carry response_format (intermediate tool round).
	assert.Empty(t, reqs[0].ResponseFormat, "intermediate tool-call round must not carry response_format")
	// Round 2 MUST carry response_format (terminal synthesis round).
	assert.NotEmpty(t, reqs[1].ResponseFormat, "terminal round must carry response_format")
}

// TestLiang_NoOutputSchema_NoResponseFormat verifies that a Liang WITHOUT
// output_schema never sets response_format (behavior unchanged from before).
func TestLiang_NoOutputSchema_NoResponseFormat(t *testing.T) {
	defer goleak.VerifyNone(t)
	client := newFake(
		fakeResponse{
			tokens:       []string{"free text answer"},
			content:      "free text answer",
			finishReason: "stop",
			usage:        Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		},
	)
	liang, err := NewLiang(config.AgentConfig{
		Name:         "Liang",
		Model:        "gpt-test",
		SystemPrompt: "you are liang",
		MaxTokens:    256,
	}, client, tool.NewRegistry())
	require.NoError(t, err)

	hub := stream.NewHub(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	assert.Empty(t, req.ResponseFormat, "no output_schema => no response_format")
}
