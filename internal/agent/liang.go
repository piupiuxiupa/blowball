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

// Liang is the analysis agent. It runs a tool-calling loop for the tools listed
// in its config (built-ins and MCP proxies), but unlike Confucius it never
// dispatches sub-agents.
type Liang struct {
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
	// hitCapThisRun records whether the most recent Run exited via the cap
	// path; exposed via LastRunHitCap so Confucius can propagate a sub-agent
	// cap into usage.meta.round_capped. Liang's Run is single-threaded and the
	// dispatcher reads this only after Run returns, so no mutex is needed.
	hitCapThisRun bool
	// responseFormat is the pre-built OpenAI response_format payload derived
	// from cfg.OutputSchema (nil/ok=false when no schema is configured). It is
	// attached to the terminal round's LLMRequest to enable structured output
	// (capability A).
	responseFormat json.RawMessage
}

// NewLiang builds a Liang agent. reg contains the tools this Liang instance is
// allowed to call, filtered by the orchestrator from the process-wide registry.
// turn is the turn-resolved model/effort/quota/continuation configuration
// applied to every LLM call this agent makes (model-effort-v2,
// per-model-completion-budget) — including the turn's finish_reason=length
// continuation policy.
func NewLiang(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, turn ModelOverride) (*Liang, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools, turn.MaxCompletionTokens)
	if err != nil {
		return nil, fmt.Errorf("agent: build liang tools: %w", err)
	}
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultAgentMaxRounds()
	}
	return &Liang{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		turn:          turn,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
		maxRounds:     maxRounds,
		responseFormat: buildResponseFormatPayload(cfg.OutputSchema, cfg.Name),
	}, nil
}

// Name implements Agent.
func (l *Liang) Name() string { return l.cfg.Name }

// SystemPrompt implements Agent.
func (l *Liang) SystemPrompt() string { return l.cfg.SystemPrompt }

// RetryPolicy implements Agent, returning the agent's configured retry policy.
func (l *Liang) RetryPolicy() config.AgentRetryConfig { return l.cfg.Retry }

// LastRunHitCap implements RoundCapTracker so Confucius can propagate a Liang
// cap into usage.meta.round_capped.
func (l *Liang) LastRunHitCap() bool { return l.hitCapThisRun }

// Run executes Liang's tool-calling loop. When no tools are configured it
// degrades to a single streaming completion, sending no tools[] field so the
// existing TestLiang_NoTools_PassesEmptyToolsJSON contract still holds.
func (l *Liang) Run(ctx context.Context, messages []Message, hub stream.EventHub) (string, Usage, *TurnBreakdown, error) {
	// Attribute every LLM call this loop makes (including round-cap wrap-up
	// rounds, which inherit this ctx) to this agent in the raw-capture log.
	ctx = WithAgentName(ctx, l.Name())

	select {
	case <-ctx.Done():
		return "", Usage{}, nil, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(l.Name())) {
		return "", Usage{}, nil, ctx.Err()
	}

	round := append([]Message{}, messages...)
	var total Usage
	var finalContent string
	l.hitCapThisRun = false
	var capped bool // set when the loop exits by hitting the round cap (not via a natural break)

	for i := 0; i < l.maxRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, nil, ctx.Err()
		default:
		}

		req := LLMRequest{
			Model:               l.turn.Model,
			Messages:            withSystem(l.cfg.SystemPrompt, round),
			MaxCompletionTokens: l.turn.MaxCompletionTokens,
			Thinking:            l.turn.Thinking,
			ReasoningEffort:     l.turn.ReasoningEffort,
		}
		if l.toolsIsNotNil {
			req.Tools = l.toolsJSON
		}
		// Structured output (capability A): when an output_schema is configured,
		// enable response_format: json_schema on the round that is expected to
		// produce the final structured answer handed back to Confucius. A round
		// is treated as terminal (synthesis) when either (a) the immediately
		// preceding context is a tool result (we just dispatched tools and the
		// model is now synthesizing), or (b) no tools are configured (the model
		// can only answer directly). The first round of a tooled agent is NOT
		// treated as terminal because the model may still emit tool_calls, and
		// response_format conflicts with tool use. A reasoning turn (effort !=
		// none) never reaches here: config load rejects output_schema with a
		// non-none default effort, and the handler 400s requests resolving to
		// effort != none (reasoning degrades to prompt-only constraints via
		// system-prompt text, not this field).
		if rf, ok := l.terminalResponseFormat(round); ok {
			req.ResponseFormat = rf
		}

		var assistantText string
		result, err := runLLMRound(ctx, l.client, hub, l.Name(), req, &round, l.turn.LengthContinue, l.cfg.Retry,
			func(r []Message) []Message { return withSystem(l.cfg.SystemPrompt, r) },
			// Parseable tool_calls of an intermediate length response dispatch
			// through the same record path as the main loop.
			func(gctx context.Context, calls []ToolCall) {
				l.executeAndRecordToolCalls(gctx, calls, hub, &round)
			},
			func(delta string) error {
				assistantText += delta
				if !hub.SendCtx(ctx, stream.TokenEvent(l.Name(), delta)) {
					return ctx.Err()
				}
				return nil
			}, func(delta string) error {
				if !hub.SendCtx(ctx, stream.ReasoningEvent(l.Name(), delta)) {
					return ctx.Err()
				}
				return nil
			})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, nil, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(l.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(l.Name()))
			return finalContent, total, nil, fmt.Errorf("liang: stream chat: %w", err)
		}

		total.Add(result.Usage)

		// llm-length-continuation exhaustion (design D7): fail loudly, keep
		// the accumulated partial content. The error text deliberately avoids
		// transient-error substrings so the sub-agent retry pipeline (which
		// Liang runs under when dispatched by Confucius) never retries it.
		if l.turn.LengthContinue.Enabled() && result.LengthHit {
			step, retries := l.turn.LengthContinue.Resolve()
			msg := lengthExhaustedMessage(l.Name(), retries, l.turn.MaxCompletionTokens+retries*step)
			hub.SendCtx(ctx, stream.AgentErrorEvent(l.Name(), msg, "length_exhausted"))
			hub.SendCtx(ctx, stream.AgentEndEvent(l.Name()))
			return result.Content, total, nil, fmt.Errorf("liang: %s", msg)
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

		l.executeAndRecordToolCalls(ctx, resp.ToolCalls, hub, &round)
		// This iteration dispatched tools and did not break; flag a cap exit
		// when it was the last allowed round so the post-loop wrap-up runs.
		capped = i+1 == l.maxRounds
	}

	// Round cap reached without a natural stop: give the model one
	// tool-disabled wrap-up round to synthesize a final answer instead of
	// ending empty. WARN log + hitCapThisRun always; agent_error only if the
	// wrap-up recovers no content (genuine empty-end).
	if capped {
		l.hitCapThisRun = true
		emitCapHitWarn(l.Name(), l.maxRounds, l.maxRounds)
		wrapReq := LLMRequest{
			Model:               l.turn.Model,
			Messages:            withSystem(l.cfg.SystemPrompt, round),
			MaxCompletionTokens: l.turn.MaxCompletionTokens,
			Thinking:            l.turn.Thinking,
			ReasoningEffort:     l.turn.ReasoningEffort,
			// Tools intentionally omitted: force a prose/structured answer.
		}
		// The wrap-up IS the terminal round, so a structured-output Liang still
		// carries response_format (the synthesized answer conforms to
		// output_schema). terminalResponseFormat returns the format when the
		// preceding context is a tool result (true here: the last dispatched
		// round appended role="tool" messages) or when no tools are configured.
		if rf, ok := l.terminalResponseFormat(round); ok {
			wrapReq.ResponseFormat = rf
		}
		wrapContent, wrapUsage, wrapErr := runWrapUpRound(ctx, l.client, l.Name(), hub, wrapReq, &round, l.turn.LengthContinue, l.cfg.Retry,
			func(r []Message) []Message {
				return append(withSystem(l.cfg.SystemPrompt, r), Message{Role: "user", Content: wrapUpInstruction})
			})
		if wrapErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, total, nil, ctxErr
			}
			emitCapExhaustedError(ctx, hub, l.Name())
			return finalContent, total, nil, fmt.Errorf("liang: round cap exhausted: %w", wrapErr)
		}
		total.Add(wrapUsage)
		if strings.TrimSpace(wrapContent) != "" {
			finalContent = wrapContent
		} else {
			emitCapExhaustedError(ctx, hub, l.Name())
			return finalContent, total, nil, fmt.Errorf("liang: round cap exhausted: wrap-up round produced no content")
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(l.Name())) {
		return finalContent, total, nil, ctx.Err()
	}
	return finalContent, total, nil, nil
}

// executeAndRecordToolCalls dispatches calls in parallel and records every
// result: one ToolResultEvent per call plus one role="tool" message appended
// to *round. Shared by the main loop and the length-continuation path
// (llm-length-continuation, D5).
func (l *Liang) executeAndRecordToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub, round *[]Message) {
	results := l.dispatchToolCalls(ctx, calls, hub)
	for _, tc := range calls {
		result, ok := results[tc.ID]
		if !ok {
			result = toolResult{content: "", isError: true}
		}
		hub.SendCtx(ctx, stream.ToolResultEvent(l.Name(), tc.ID, result.content))
		*round = append(*round, Message{
			Role:       "tool",
			Content:    result.content,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
		})
	}
}

// dispatchToolCalls runs every tool_call in parallel through the tool registry.
// Liang never dispatches sub-agents, so invoke_* tool names fall through and
// error as unknown tools.
func (l *Liang) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub) map[string]toolResult {
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
func (l *Liang) dispatchOneRegistryTool(ctx context.Context, tc ToolCall, hub stream.EventHub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(l.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if l.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, l.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	// On a tool failure the status envelope is the sole channel — no agent_error
	// is emitted for tool failures (capability: tool-result-envelope).
	out, err := l.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	return toolResult{content: renderToolResult(out, err), isError: err != nil}
}

// terminalResponseFormat returns the response_format payload to attach to this
// round's request when output_schema is configured and the round is the
// terminal/synthesis round. A round is terminal when the most recent message in
// the working conversation is a tool result (we just dispatched tools and the
// model is synthesizing) OR when no tools are configured (the model can only
// answer directly on the first round). The first round of a tooled agent is
// NOT terminal (the model may still emit tool_calls). Returns (nil, false)
// when output_schema is unset or the round is non-terminal.
func (l *Liang) terminalResponseFormat(round []Message) (json.RawMessage, bool) {
	if len(l.responseFormat) == 0 {
		return nil, false
	}
	// No tools configured: the first (and likely only) round is terminal.
	if !l.toolsIsNotNil {
		return l.responseFormat, true
	}
	// Tooled agent: terminal only when the preceding context is a tool result
	// (we are synthesizing after a tool dispatch).
	if len(round) > 0 && round[len(round)-1].Role == "tool" {
		return l.responseFormat, true
	}
	return nil, false
}

// buildResponseFormatPayload wraps a raw JSON Schema (config OutputSchema) into
// the full OpenAI response_format wire shape used by structured output. The
// agent's display name (sanitized) becomes the json_schema name. Returns nil
// when schemaJSON is empty. The schema is embedded verbatim under
// json_schema.schema with strict:true so the model adheres exactly.
func buildResponseFormatPayload(schemaJSON, agentName string) json.RawMessage {
	if strings.TrimSpace(schemaJSON) == "" {
		return nil
	}
	name := sanitizeSchemaName(agentName)
	payload := map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   name,
			"strict": true,
			"schema": json.RawMessage(schemaJSON),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// Should not happen: schemaJSON is validated as JSON at config load.
		return nil
	}
	return raw
}

// sanitizeSchemaName reduces an agent display name to the [a-zA-Z0-9_-] charset
// OpenAI requires for the json_schema name (max 64 chars), defaulting to
// "result" when nothing usable remains.
func sanitizeSchemaName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "result"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
