package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"golang.org/x/sync/errgroup"
)

// maxLiangRounds bounds Liang's tool-calling loop. Analysis tasks occasionally
// need to fetch context with webfetch or query an MCP tool, but they should
// converge quickly.
const maxLiangRounds = 16

// Liang is the analysis agent. It runs a tool-calling loop for the tools listed
// in its config (built-ins and MCP proxies), but unlike Confuse it never
// dispatches sub-agents.
type Liang struct {
	cfg           config.AgentConfig
	client        LLMClient
	toolRegistry  *tool.Registry
	toolsJSON     []byte
	toolsIsNotNil bool
}

// NewLiang builds a Liang agent. reg contains the tools this Liang instance is
// allowed to call, filtered by the orchestrator from the process-wide registry.
func NewLiang(cfg config.AgentConfig, client LLMClient, reg *tool.Registry) (*Liang, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build liang tools: %w", err)
	}
	return &Liang{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
	}, nil
}

// Name implements Agent.
func (l *Liang) Name() string { return l.cfg.Name }

// SystemPrompt implements Agent.
func (l *Liang) SystemPrompt() string { return l.cfg.SystemPrompt }

// Run executes Liang's tool-calling loop. When no tools are configured it
// degrades to a single streaming completion, sending no tools[] field so the
// existing TestLiang_NoTools_PassesEmptyToolsJSON contract still holds.
func (l *Liang) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, error) {
	select {
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(l.Name())) {
		return "", Usage{}, ctx.Err()
	}

	round := append([]Message{}, messages...)
	var total Usage
	var finalContent string

	for i := 0; i < maxLiangRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, ctx.Err()
		default:
		}

		req := LLMRequest{
			Model:           l.cfg.Model,
			Messages:        withSystem(l.cfg.SystemPrompt, round),
			MaxTokens:       l.cfg.MaxTokens,
			Thinking:        l.cfg.Thinking,
			ReasoningEffort: l.cfg.ReasoningEffort,
		}
		if l.toolsIsNotNil {
			req.Tools = l.toolsJSON
		}

		var assistantText string
		resp, err := l.client.StreamChat(ctx, req, func(delta string) error {
			assistantText += delta
			if !hub.SendCtx(ctx, stream.TokenEvent(l.Name(), delta)) {
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(l.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(l.Name()))
			return finalContent, total, fmt.Errorf("liang: stream chat: %w", err)
		}

		total.Add(resp.Usage)

		assistantMsg := Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && len(resp.ToolCalls) == 0 {
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) == 0 {
			finalContent = resp.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		results := l.dispatchToolCalls(ctx, resp.ToolCalls, hub)
		for _, tc := range resp.ToolCalls {
			result, ok := results[tc.ID]
			if !ok {
				result = toolResult{content: "", isError: true}
			}
			hub.SendCtx(ctx, stream.ToolResultEvent(l.Name(), tc.ID, result.content))
			round = append(round, Message{
				Role:       "tool",
				Content:    result.content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(l.Name())) {
		return finalContent, total, ctx.Err()
	}
	return finalContent, total, nil
}

// dispatchToolCalls runs every tool_call in parallel through the tool registry.
// Liang never dispatches sub-agents, so invoke_* tool names fall through and
// error as unknown tools.
func (l *Liang) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub *stream.Hub) map[string]toolResult {
	results := make(map[string]toolResult, len(calls))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for _, tc := range calls {
		tc := tc
		g.Go(func() error {
			res := l.dispatchOneRegistryTool(gctx, tc, hub)
			mu.Lock()
			results[tc.ID] = res
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// dispatchOneRegistryTool routes a single tool_call straight to the tool
// registry. invoke_* names will return "unknown tool" because the registry
// has no such entry.
func (l *Liang) dispatchOneRegistryTool(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(l.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if l.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, l.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	out, err := l.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if err != nil {
		msg := err.Error()
		streamAgentError(hub, ctx, l.Name(), msg, "tool_error")
		return toolResult{content: msg, isError: true}
	}
	return toolResult{content: marshalToolResult(out)}
}
