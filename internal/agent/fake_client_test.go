package agent

import (
	"context"
	"sync"

	"github.com/lush/blowball/internal/stream"
)

// fakeLLMClient implements LLMClient for tests. Each call to StreamChat pops
// the next prepared response off responses (FIFO), invokes onToken for each
// token in that response, and records the request so tests can assert on
// Tools / Messages. A panic is raised if a test exhausts responses without
// adding enough — that is always a test-authoring bug.
type fakeLLMClient struct {
	mu        sync.Mutex
	responses []fakeResponse
	calls     []LLMRequest
}

type fakeResponse struct {
	content      string
	tokens       []string // streamed token-by-token via onToken
	finishReason string
	toolCalls    []ToolCall
	usage        Usage
	err          error // if non-nil, StreamChat returns this without emitting
}

func newFake(responses ...fakeResponse) *fakeLLMClient {
	return &fakeLLMClient{responses: responses}
}

func (f *fakeLLMClient) StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error) (LLMResponse, error) {
	f.mu.Lock()
	if len(f.responses) == 0 {
		f.mu.Unlock()
		panic("fakeLLMClient: no responses queued; test forgot to enqueue one")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	if resp.err != nil {
		return LLMResponse{}, resp.err
	}

	for _, tok := range resp.tokens {
		if err := ctx.Err(); err != nil {
			return LLMResponse{FinishReason: resp.finishReason, Content: resp.content, ToolCalls: resp.toolCalls, Usage: resp.usage}, err
		}
		if onToken != nil {
			if err := onToken(tok); err != nil {
				return LLMResponse{FinishReason: resp.finishReason, Content: resp.content, ToolCalls: resp.toolCalls, Usage: resp.usage}, err
			}
		}
	}

	return LLMResponse{
		FinishReason: resp.finishReason,
		Content:      resp.content,
		ToolCalls:    resp.toolCalls,
		Usage:        resp.usage,
	}, nil
}

func (f *fakeLLMClient) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLLMClient) lastRequest() LLMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return LLMRequest{}
	}
	return f.calls[len(f.calls)-1]
}

// fakeAgent is a stand-in Agent for sub-agent dispatch tests. It records its
// invocations and emits a deterministic token stream + lifecycle events so
// tests can verify that the parent loop wired sub-agent events through the hub.
type fakeAgent struct {
	name    string
	prompt  string
	content string
	tokens  []string
	usage   Usage
	err     error
	mu      sync.Mutex
	calls   []fakeAgentCall
}

type fakeAgentCall struct {
	messages []Message
}

func (a *fakeAgent) Name() string         { return a.name }
func (a *fakeAgent) SystemPrompt() string { return a.prompt }

func (a *fakeAgent) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, error) {
	a.mu.Lock()
	a.calls = append(a.calls, fakeAgentCall{messages: append([]Message{}, messages...)})
	a.mu.Unlock()

	if !hub.SendCtx(ctx, stream.AgentStartEvent(a.name)) {
		return "", Usage{}, ctx.Err()
	}
	if a.err != nil {
		hub.SendCtx(ctx, stream.AgentErrorEvent(a.name, a.err.Error(), "agent_failed"))
		hub.SendCtx(ctx, stream.AgentEndEvent(a.name))
		return "", Usage{}, a.err
	}
	for _, t := range a.tokens {
		if err := ctx.Err(); err != nil {
			return "", Usage{}, err
		}
		if !hub.SendCtx(ctx, stream.TokenEvent(a.name, t)) {
			return "", Usage{}, ctx.Err()
		}
	}
	if !hub.SendCtx(ctx, stream.AgentEndEvent(a.name)) {
		return "", Usage{}, ctx.Err()
	}
	return a.content, a.usage, nil
}

func (a *fakeAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}
