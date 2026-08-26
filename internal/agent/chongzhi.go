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
	cfg          config.AgentConfig
	client       LLMClient
	toolRegistry *tool.Registry
	// turn is the turn-level model/effort configuration (model-effort-v2),
	// injected at construction — agents no longer carry model fields. See
	// Confucius.turn.
	turn          ModelOverride
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

// NewChongzhi builds a Chongzhi agent. turn is the turn-resolved
// model/effort/quota/continuation configuration applied to every LLM call
// this agent makes (model-effort-v2, per-model-completion-budget) — including
// the write-budget number injected into the tools[] descriptions and the
// turn's finish_reason=length continuation policy.
func NewChongzhi(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, turn ModelOverride) (*Chongzhi, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools, turn.MaxCompletionTokens)
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
		turn:          turn,
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
func (c *Chongzhi) Run(ctx context.Context, messages []Message, hub stream.EventHub) (string, Usage, *TurnBreakdown, error) {
	// Attribute every LLM call this loop makes (including round-cap wrap-up
	// rounds, which inherit this ctx) to this agent in the raw-capture log.
	ctx = WithAgentName(ctx, c.Name())

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
			Model:               c.turn.Model,
			Messages:            withSystem(c.cfg.SystemPrompt, round),
			MaxCompletionTokens: c.turn.MaxCompletionTokens,
			Thinking:            c.turn.Thinking,
			ReasoningEffort:     c.turn.ReasoningEffort,
		}
		if c.toolsIsNotNil {
			req.Tools = c.toolsJSON
		}

		var assistantText string
		result, err := runLLMRound(ctx, c.client, hub, c.Name(), req, &round, c.turn.LengthContinue, c.cfg.Retry,
			func(r []Message) []Message { return withSystem(c.cfg.SystemPrompt, r) },
			// Parseable tool_calls of an intermediate length response dispatch
			// through the same record path as the main loop (including the
			// executedToolThisRun side-effect flag — a dispatched call during
			// continuation IS a side effect).
			func(gctx context.Context, calls []ToolCall) {
				c.executeAndRecordToolCalls(gctx, calls, hub, &round)
			},
			func(delta string) error {
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
			// The error return value carries the failed round's partial
			// output (tokens already streamed into assistantText), NOT the
			// always-empty finalContent, so dispatchSubAgent's give-up point
			// can join it with the error text (subagent-partial-output-on-
			// failure).
			if ctxErr := ctx.Err(); ctxErr != nil {
				return assistantText, total, nil, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return assistantText, total, nil, fmt.Errorf("chongzhi: stream chat: %w", err)
		}

		total.Add(result.Usage)

		// llm-length-continuation exhaustion (design D7): fail loudly, keep
		// the accumulated partial content. The error text deliberately avoids
		// transient-error substrings so the sub-agent retry pipeline (which
		// Chongzhi runs under when dispatched by Confucius) never retries it.
		if c.turn.LengthContinue.Enabled() && result.LengthHit {
			step, retries := c.turn.LengthContinue.Resolve()
			msg := lengthExhaustedMessage(c.Name(), retries, c.turn.MaxCompletionTokens+retries*step)
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), msg, "length_exhausted"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return result.Content, total, nil, fmt.Errorf("chongzhi: %s", msg)
		}

		resp := result.Resp
		assistantMsg := Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && assistantMsg.ReasoningContent == "" && len(resp.ToolCalls) == 0 {
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		if !shouldDispatchToolCalls(resp) {
			finalContent = result.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		c.executeAndRecordToolCalls(ctx, resp.ToolCalls, hub, &round)
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
			Model:               c.turn.Model,
			Messages:            withSystem(c.cfg.SystemPrompt, round),
			MaxCompletionTokens: c.turn.MaxCompletionTokens,
			Thinking:            c.turn.Thinking,
			ReasoningEffort:     c.turn.ReasoningEffort,
			// Tools intentionally omitted: force a prose answer, no dispatch.
		}
		wrapContent, wrapUsage, wrapErr := runWrapUpRound(ctx, c.client, c.Name(), hub, wrapReq, &round, c.turn.LengthContinue, c.cfg.Retry,
			func(r []Message) []Message {
				return append(withSystem(c.cfg.SystemPrompt, r), Message{Role: "user", Content: wrapUpInstruction})
			})
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

// executeAndRecordToolCalls dispatches calls in parallel and records every
// result: one ToolResultEvent per call plus one role="tool" message appended
// to *round, and the executedToolThisRun side-effect flag when any call
// succeeded (capability C idempotency). Shared by the main loop and the
// length-continuation path (llm-length-continuation, D5).
func (c *Chongzhi) executeAndRecordToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub, round *[]Message) {
	results := c.dispatchToolCalls(ctx, calls, hub)
	for _, tc := range calls {
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
		*round = append(*round, Message{
			Role:       "tool",
			Content:    result.content,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
		})
	}
}

// dispatchToolCalls runs every tool_call in parallel. Unlike Confucius, there
// is NO sub-agent interception — invoke_* tool names fall through to the
// registry and error as "unknown tool", enforcing the flat topology.
func (c *Chongzhi) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub) map[string]toolResult {
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
func (c *Chongzhi) dispatchOneRegistryTool(ctx context.Context, tc ToolCall, hub stream.EventHub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(c.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if c.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, c.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	// On a tool failure the status envelope is the sole channel — no agent_error
	// is emitted for tool failures (capability: tool-result-envelope).
	out, err := c.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	return toolResult{content: renderToolResult(out, err), isError: err != nil}
}
