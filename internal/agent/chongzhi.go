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

// maxChongzhiRounds bounds Chongzhi's tool-calling loop. Coding tasks tend to
// need more rounds than Confuse (read → write → verify), so the cap is more
// generous than the orchestrator's.
const maxChongzhiRounds = 32

// Chongzhi is the coding agent. It runs with Xizhi file tools configured via
// cfg.Tools and dispatches tool_calls straight to the tool registry. Per the
// flat-topology contract, Chongzhi NEVER dispatches sub-agents — even if a
// model emitted an invoke_* tool name, dispatchOneRegistryTool would error
// with "unknown tool" rather than recurse. This is enforced structurally:
// Chongzhi does not hold a subAgents map and its dispatch path only consults
// the registry.
type Chongzhi struct {
	cfg           config.AgentConfig
	client        LLMClient
	toolRegistry  *tool.Registry
	toolsJSON     []byte
	toolsIsNotNil bool
}

// NewChongzhi builds a Chongzhi agent.
func NewChongzhi(cfg config.AgentConfig, client LLMClient, reg *tool.Registry) (*Chongzhi, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build chongzhi tools: %w", err)
	}
	return &Chongzhi{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
	}, nil
}

// Name implements Agent.
func (c *Chongzhi) Name() string { return c.cfg.Name }

// SystemPrompt implements Agent.
func (c *Chongzhi) SystemPrompt() string { return c.cfg.SystemPrompt }

// Run executes the Chongzhi agent loop with streaming and tool dispatch.
func (c *Chongzhi) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, error) {
	select {
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(c.Name())) {
		return "", Usage{}, ctx.Err()
	}

	round := append([]Message{}, messages...)
	var total Usage
	var finalContent string

	for i := 0; i < maxChongzhiRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, ctx.Err()
		default:
		}

		req := LLMRequest{
			Model:     c.cfg.Model,
			Messages:  withSystem(c.cfg.SystemPrompt, round),
			MaxTokens: c.cfg.MaxTokens,
		}
		if c.toolsIsNotNil {
			req.Tools = c.toolsJSON
		}

		var assistantText string
		resp, err := c.client.StreamChat(ctx, req, func(delta string) error {
			assistantText += delta
			if !hub.SendCtx(ctx, stream.TokenEvent(c.Name(), delta)) {
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return finalContent, total, fmt.Errorf("chongzhi: stream chat: %w", err)
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

		results := c.dispatchToolCalls(ctx, resp.ToolCalls, hub)
		for _, tc := range resp.ToolCalls {
			result, ok := results[tc.ID]
			if !ok {
				result = toolResult{content: "", isError: true}
			}
			round = append(round, Message{
				Role:       "tool",
				Content:    result.content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(c.Name())) {
		return finalContent, total, ctx.Err()
	}
	return finalContent, total, nil
}

// dispatchToolCalls runs every tool_call in parallel. Unlike Confuse, there
// is NO sub-agent interception — invoke_* tool names fall through to the
// registry and error as "unknown tool", enforcing the flat topology.
func (c *Chongzhi) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub *stream.Hub) map[string]toolResult {
	results := make(map[string]toolResult, len(calls))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for _, tc := range calls {
		tc := tc
		g.Go(func() error {
			res := c.dispatchOneRegistryTool(gctx, tc, hub)
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
// has no such entry (sub-agent tools are never registered).
func (c *Chongzhi) dispatchOneRegistryTool(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(c.Name(), tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if c.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, c.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	out, err := c.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if err != nil {
		msg := err.Error()
		streamAgentError(hub, ctx, c.Name(), msg, "tool_error")
		return toolResult{content: msg, isError: true}
	}
	return toolResult{content: marshalToolResult(out)}
}
