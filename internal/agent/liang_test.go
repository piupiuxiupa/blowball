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
		_, _, _ = liang.Run(ctx, []Message{{Role: "user", Content: "hello"}}, hub)
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

	content, usage, err := liang.Run(ctx, []Message{{Role: "user", Content: "ping"}}, hub)
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
		c, u, e := liang.Run(ctx, []Message{{Role: "user", Content: "count"}}, hub)
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
	_, _, err = liang.Run(ctx, []Message{{Role: "user", Content: "analyze"}}, hub)
	require.NoError(t, err)
	hub.Close()

	req := client.lastRequest()
	assert.True(t, req.Thinking, "Thinking must be true")
	assert.Equal(t, "low", req.ReasoningEffort)
	assert.Equal(t, 512, req.MaxTokens)
}
