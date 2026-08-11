package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"golang.org/x/sync/errgroup"
)

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
	// maxRounds bounds the tool-calling loop; resolved from cfg.MaxRounds
	// (default config.DefaultAgentMaxRounds) at construction.
	maxRounds int
	// runMu guards the two "last Run" flags below, which are read by the
	// dispatcher (Confucius) / retry wrapper from a separate goroutine after
	// Run returns.
	runMu sync.Mutex
	// executedToolThisRun records whether the most recent Run dispatched at
	// least one successful tool call (capability C idempotency: a
	// side-effecting agent is retried only before it touches the file system).
	executedToolThisRun bool
	// hitCapThisRun records whether the most recent Run exited via the cap
	// path; exposed via LastRunHitCap so Confucius can propagate a sub-agent
	// cap into usage.meta.round_capped.
	hitCapThisRun bool
}

// NewChongzhi builds a Chongzhi agent.
func NewChongzhi(cfg config.AgentConfig, client LLMClient, reg *tool.Registry) (*Chongzhi, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build chongzhi tools: %w", err)
	}
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultAgentMaxRounds()
	}
	return &Chongzhi{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
		maxRounds:     maxRounds,
	}, nil
}

// Name implements Agent.
func (c *Chongzhi) Name() string { return c.cfg.Name }

// SystemPrompt implements Agent.
func (c *Chongzhi) SystemPrompt() string { return c.cfg.SystemPrompt }

// RetryPolicy implements Agent, returning the agent's configured retry policy.
func (c *Chongzhi) RetryPolicy() config.AgentRetryConfig { return c.cfg.Retry }

// Run executes the Chongzhi agent loop with streaming and tool dispatch.
func (c *Chongzhi) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, *TurnBreakdown, error) {
	select {
	case <-ctx.Done():
		return "", Usage{}, nil, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(c.Name())) {
		return "", Usage{}, nil, ctx.Err()
	}

	round := append([]Message{}, messages...)
	var total Usage
	var finalContent string
	c.runMu.Lock()
	c.executedToolThisRun = false
	c.hitCapThisRun = false
	c.runMu.Unlock()
	var capped bool // set when the loop exits by hitting the round cap (not via a natural break)

	for i := 0; i < c.maxRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, nil, ctx.Err()
		default:
		}

		req := LLMRequest{
			Model:           c.cfg.Model,
			Messages:        withSystem(c.cfg.SystemPrompt, round),
			MaxTokens:       c.cfg.MaxTokens,
			Thinking:        c.cfg.Thinking,
			ReasoningEffort: c.cfg.ReasoningEffort,
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
		}, func(delta string) error {
			if !hub.SendCtx(ctx, stream.ReasoningEvent(c.Name(), delta)) {
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, nil, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return finalContent, total, nil, fmt.Errorf("chongzhi: stream chat: %w", err)
		}

		total.Add(resp.Usage)

		assistantMsg := Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && assistantMsg.ReasoningContent == "" && len(resp.ToolCalls) == 0 {
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		if !shouldDispatchToolCalls(resp) {
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
			// A non-error tool result means the tool executed successfully; mark
			// the Run as having produced a side effect so the retry wrapper can
			// suppress retries after this point (capability C idempotency).
			if !result.isError {
				c.runMu.Lock()
				c.executedToolThisRun = true
				c.runMu.Unlock()
			}
			hub.SendCtx(ctx, stream.ToolResultEvent(c.Name(), tc.ID, result.content))
			round = append(round, Message{
				Role:       "tool",
				Content:    result.content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
		// This iteration dispatched tools and did not break; flag a cap exit
		// when it was the last allowed round so the post-loop wrap-up runs.
		capped = i+1 == c.maxRounds
	}

	// Round cap reached without a natural stop: give the model one
	// tool-disabled wrap-up round to synthesize a final answer instead of
	// ending empty. WARN log + hitCapThisRun always; agent_error only if the
	// wrap-up recovers no content (genuine empty-end).
	if capped {
		c.runMu.Lock()
		c.hitCapThisRun = true
		c.runMu.Unlock()
		emitCapHitWarn(c.Name(), c.maxRounds, c.maxRounds)
		wrapReq := LLMRequest{
			Model:           c.cfg.Model,
			Messages:        withSystem(c.cfg.SystemPrompt, round),
			MaxTokens:       c.cfg.MaxTokens,
			Thinking:        c.cfg.Thinking,
			ReasoningEffort: c.cfg.ReasoningEffort,
			// Tools intentionally omitted: force a prose answer, no dispatch.
		}
		wrapContent, wrapUsage, wrapErr := runWrapUpRound(ctx, c.client, c.Name(), hub, wrapReq)
		if wrapErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, nil, ctxErr
			}
			emitCapExhaustedError(ctx, hub, c.Name())
			return finalContent, total, nil, fmt.Errorf("chongzhi: round cap exhausted: %w", wrapErr)
		}
		total.Add(wrapUsage)
		if strings.TrimSpace(wrapContent) != "" {
			finalContent = wrapContent
		} else {
			emitCapExhaustedError(ctx, hub, c.Name())
			return finalContent, total, nil, fmt.Errorf("chongzhi: round cap exhausted: wrap-up round produced no content")
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(c.Name())) {
		return finalContent, total, nil, ctx.Err()
	}
	return finalContent, total, nil, nil
}

// LastRunExecutedTool implements ToolCallTracker. It reports whether the most
// recent Run dispatched at least one successful tool call, so the retry
// wrapper can suppress retries after Chongzhi has produced a file-system side
// effect (capability C idempotency).
func (c *Chongzhi) LastRunExecutedTool() bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return c.executedToolThisRun
}

// LastRunHitCap implements RoundCapTracker so Confucius can propagate a
// Chongzhi cap into usage.meta.round_capped. Guarded by runMu alongside
// executedToolThisRun (read by the dispatcher after Run returns).
func (c *Chongzhi) LastRunHitCap() bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return c.hitCapThisRun
}

// dispatchToolCalls runs every tool_call in parallel. Unlike Confucius, there
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
	if !hub.SendCtx(ctx, stream.ToolCallEvent(c.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if c.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, c.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	out, err := c.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if err != nil {
		// Frontend channel: agent_error still fires; the model-facing error is
		// carried in-band by the status envelope (capability: tool-result-envelope).
		streamAgentError(hub, ctx, c.Name(), err.Error(), "tool_error")
	}
	return toolResult{content: renderToolResult(out, err), isError: err != nil}
}
