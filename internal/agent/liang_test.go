package agent

import (
	"context"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
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
	}, client)
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
	}, client)
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
