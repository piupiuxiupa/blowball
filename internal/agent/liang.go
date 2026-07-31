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

// maxLiangRounds bounds Liang's tool-calling loop. Analysis tasks occasionally
// need to fetch context with webfetch or query an MCP tool, but they should
// converge quickly.
const maxLiangRounds = 16

// Liang is the analysis agent. It runs a tool-calling loop for the tools listed
// in its config (built-ins and MCP proxies), but unlike Confucius it never
// dispatches sub-agents.
type Liang struct {
	cfg           config.AgentConfig
	client        LLMClient
	toolRegistry  *tool.Registry
	toolsJSON     []byte
	toolsIsNotNil bool
	// responseFormat is the pre-built OpenAI response_format payload derived
	// from cfg.OutputSchema (nil/ok=false when no schema is configured). It is
	// attached to the terminal round's LLMRequest to enable structured output
	// (capability A).
	responseFormat json.RawMessage
}

// NewLiang builds a Liang agent. reg contains the tools this Liang instance is
// allowed to call, filtered by the orchestrator from the process-wide registry.
func NewLiang(cfg config.AgentConfig, client LLMClient, reg *tool.Registry) (*Liang, error) {
	toolsJSON, err := buildRegularToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build liang tools: %w", err)
	}
	return &Liang{
		cfg:            cfg,
		client:         client,
		toolRegistry:   reg,
		toolsJSON:      toolsJSON,
		toolsIsNotNil:  len(toolsJSON) > 0 && string(toolsJSON) != "null",
		responseFormat: buildResponseFormatPayload(cfg.OutputSchema, cfg.Name),
	}, nil
}

// Name implements Agent.
func (l *Liang) Name() string { return l.cfg.Name }

// SystemPrompt implements Agent.
func (l *Liang) SystemPrompt() string { return l.cfg.SystemPrompt }

// RetryPolicy implements Agent, returning the agent's configured retry policy.
func (l *Liang) RetryPolicy() config.AgentRetryConfig { return l.cfg.Retry }

// Run executes Liang's tool-calling loop. When no tools are configured it
// degrades to a single streaming completion, sending no tools[] field so the
// existing TestLiang_NoTools_PassesEmptyToolsJSON contract still holds.
func (l *Liang) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, *TurnBreakdown, error) {
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

	for i := 0; i < maxLiangRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, nil, ctx.Err()
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
		// Structured output (capability A): when an output_schema is configured,
		// enable response_format: json_schema on the round that is expected to
		// produce the final structured answer handed back to Confucius. A round
		// is treated as terminal (synthesis) when either (a) the immediately
		// preceding context is a tool result (we just dispatched tools and the
		// model is now synthesizing), or (b) no tools are configured (the model
		// can only answer directly). The first round of a tooled agent is NOT
		// treated as terminal because the model may still emit tool_calls, and
		// response_format conflicts with tool use. thinking:true never reaches
		// here: config validation rejects output_schema+thinking (reasoning
		// models degrade to prompt-only constraints via system-prompt text, not
		// this field).
		if rf, ok := l.terminalResponseFormat(round); ok {
			req.ResponseFormat = rf
		}

		var assistantText string
		resp, err := l.client.StreamChat(ctx, req, func(delta string) error {
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
		return finalContent, total, nil, ctx.Err()
	}
	return finalContent, total, nil, nil
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
