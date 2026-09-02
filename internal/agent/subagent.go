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

// SubAgent is the generic dynamic sub-agent. Its capability is entirely
// supplied by SubAgentSpec: a task-specific prompt shell, a narrowed registry,
// a round cap, and — when depth remains — recursive spawn interception.
type SubAgent struct {
	spec         SubAgentSpec
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
	// runMu guards the per-run side-effect, cap, and history fields read by
	// the dispatcher after Run returns.
	runMu               sync.Mutex
	executedToolThisRun bool
	hitCapThisRun       bool
	lastMessages        []Message
	ownUsage            Usage
	// responseFormat is the pre-built OpenAI response_format payload derived
	// from cfg.OutputSchema (nil/ok=false when no schema is configured). It is
	// attached to the terminal round's LLMRequest to enable structured output
	// (capability A).
	responseFormat json.RawMessage
}

// NewSubAgent builds a per-invocation generic sub-agent. Zero MaxRounds falls
// back to the deployment default. spec.Coordinator may be nil in focused unit
// tests; depth exhaustion then naturally leaves the synthetic spawn tool absent.
func NewSubAgent(spec SubAgentSpec, client LLMClient) (*SubAgent, error) {
	maxRounds := spec.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultAgentMaxRounds()
	}
	allowSpawn := spec.Coordinator != nil && spec.Depth < spec.Coordinator.cfg.MaxDepth
	toolsJSON, err := buildSubAgentToolsJSON(spec.ToolRegistry, spec.Tools, allowSpawn, spec.Turn.MaxCompletionTokens)
	if err != nil {
		return nil, fmt.Errorf("agent: build sub-agent tools: %w", err)
	}
	return &SubAgent{
		spec:           spec,
		client:         client,
		toolRegistry:   spec.ToolRegistry,
		turn:           spec.Turn,
		toolsJSON:      toolsJSON,
		toolsIsNotNil:  len(toolsJSON) > 0 && string(toolsJSON) != "null",
		maxRounds:      maxRounds,
		responseFormat: buildResponseFormatPayload(spec.OutputSchema, sanitizeSchemaName(spec.Name)),
	}, nil
}

// Name implements Agent.
func (s *SubAgent) Name() string { return s.spec.Name }

// SystemPrompt implements Agent.
func (s *SubAgent) SystemPrompt() string { return s.spec.SystemPrompt }

// RetryPolicy implements Agent, returning the agent's configured retry policy.
func (s *SubAgent) RetryPolicy() config.AgentRetryConfig { return s.spec.Retry }

// LastRunHitCap implements RoundCapTracker so Confucius can propagate a SubAgent
// cap into usage.meta.round_capped.
func (s *SubAgent) LastRunHitCap() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.hitCapThisRun
}

// LastRunExecutedTool implements ToolCallTracker for every generic sub-agent:
// any successfully executed tool may have side effects.
func (s *SubAgent) LastRunExecutedTool() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.executedToolThisRun
}

// LastRunMessages returns a copy of the chat history accumulated by the most
// recent Run, excluding the system prompt. The dispatcher adds the original
// system message before persisting the authoritative resume snapshot.
func (s *SubAgent) LastRunMessages() []Message {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := make([]Message, len(s.lastMessages))
	copy(out, s.lastMessages)
	return out
}

// LastRunOwnUsage returns this instance's direct LLM usage, excluding nested
// descendant usage. Run's returned usage remains the subtree aggregate used by
// the turn total; this capability lets the coordinator build by_agent without
// double-counting children.
func (s *SubAgent) LastRunOwnUsage() Usage {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.ownUsage
}

// Run executes SubAgent's tool-calling loop. When no tools are configured it
// degrades to a single streaming completion, sending no tools[] field so the
// existing TestSubAgent_NoTools_PassesEmptyToolsJSON contract still holds.
func (s *SubAgent) Run(ctx context.Context, messages []Message, hub stream.EventHub) (assistantContent string, usage Usage, breakdown *TurnBreakdown, err error) {
	// Attribute every LLM call this loop makes (including round-cap wrap-up
	// rounds, which inherit this ctx) to this agent in the raw-capture log.
	ctx = WithAgentName(ctx, s.Name())

	select {
	case <-ctx.Done():
		return "", Usage{}, nil, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(s.Name())) {
		return "", Usage{}, nil, ctx.Err()
	}

	round := append([]Message{}, messages...)
	s.runMu.Lock()
	s.executedToolThisRun = false
	s.hitCapThisRun = false
	s.lastMessages = nil
	s.ownUsage = Usage{}
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.lastMessages = append([]Message(nil), round...)
		s.runMu.Unlock()
	}()
	var finalContent string
	var capped bool // set when the loop exits by hitting the round cap (not via a natural break)

	for i := 0; i < s.maxRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, usage, nil, ctx.Err()
		default:
		}

		req := LLMRequest{
			Model:               s.turn.Model,
			Messages:            withSystem(s.spec.SystemPrompt, round),
			MaxCompletionTokens: s.turn.MaxCompletionTokens,
			Thinking:            s.turn.Thinking,
			ReasoningEffort:     s.turn.ReasoningEffort,
		}
		if s.toolsIsNotNil {
			req.Tools = s.toolsJSON
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
		if rf, ok := s.terminalResponseFormat(round); ok {
			req.ResponseFormat = rf
		}

		var assistantText string
		result, err := runLLMRound(ctx, s.client, hub, s.Name(), req, &round, s.turn.LengthContinue, s.spec.Retry,
			func(r []Message) []Message { return withSystem(s.spec.SystemPrompt, r) },
			// Parseable tool_calls of an intermediate length response dispatch
			// through the same record path as the main loop.
			func(gctx context.Context, calls []ToolCall) {
				s.executeAndRecordToolCalls(gctx, calls, hub, &round, &usage)
			},
			func(delta string) error {
				assistantText += delta
				if !hub.SendCtx(ctx, stream.TokenEvent(s.Name(), delta)) {
					return ctx.Err()
				}
				return nil
			}, func(delta string) error {
				if !hub.SendCtx(ctx, stream.ReasoningEvent(s.Name(), delta)) {
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
				return assistantText, usage, nil, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(s.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(s.Name()))
			return assistantText, usage, nil, fmt.Errorf("subagent: stream chat: %w", err)
		}

		usage.Add(result.Usage)
		s.runMu.Lock()
		s.ownUsage.Add(result.Usage)
		s.runMu.Unlock()

		// llm-length-continuation exhaustion (design D7): fail loudly, keep
		// the accumulated partial content. The error text deliberately avoids
		// transient-error substrings so the sub-agent retry pipeline (which
		// SubAgent runs under when dispatched by Confucius) never retries it.
		if s.turn.LengthContinue.Enabled() && result.LengthHit {
			step, retries := s.turn.LengthContinue.Resolve()
			msg := lengthExhaustedMessage(s.Name(), retries, s.turn.MaxCompletionTokens+retries*step)
			hub.SendCtx(ctx, stream.AgentErrorEvent(s.Name(), msg, "length_exhausted"))
			hub.SendCtx(ctx, stream.AgentEndEvent(s.Name()))
			return result.Content, usage, nil, fmt.Errorf("subagent: %s", msg)
		}

		resp := result.Resp
		assistantMsg := Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && assistantMsg.ReasoningContent == "" && len(resp.ToolCalls) == 0 {
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		if !shouldDispatchToolCalls(ctx, resp) {
			finalContent = result.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		s.executeAndRecordToolCalls(ctx, resp.ToolCalls, hub, &round, &usage)
		// This iteration dispatched tools and did not break; flag a cap exit
		// when it was the last allowed round so the post-loop wrap-up runs.
		capped = i+1 == s.maxRounds
	}

	// Round cap reached without a natural stop: give the model one
	// tool-disabled wrap-up round to synthesize a final answer instead of
	// ending empty. WARN log + hitCapThisRun always; agent_error only if the
	// wrap-up recovers no content (genuine empty-end).
	if capped {
		s.runMu.Lock()
		s.hitCapThisRun = true
		s.runMu.Unlock()
		emitCapHitWarn(ctx, s.Name(), s.maxRounds, s.maxRounds)
		wrapReq := LLMRequest{
			Model:               s.turn.Model,
			Messages:            withSystem(s.spec.SystemPrompt, round),
			MaxCompletionTokens: s.turn.MaxCompletionTokens,
			Thinking:            s.turn.Thinking,
			ReasoningEffort:     s.turn.ReasoningEffort,
			// Tools intentionally omitted: force a prose/structured answer.
		}
		// The wrap-up IS the terminal round, so a structured-output SubAgent still
		// carries response_format (the synthesized answer conforms to
		// output_schema). terminalResponseFormat returns the format when the
		// preceding context is a tool result (true here: the last dispatched
		// round appended role="tool" messages) or when no tools are configured.
		if rf, ok := s.terminalResponseFormat(round); ok {
			wrapReq.ResponseFormat = rf
		}
		wrapContent, wrapUsage, wrapErr := runWrapUpRound(ctx, s.client, s.Name(), hub, wrapReq, &round, s.turn.LengthContinue, s.spec.Retry,
			func(r []Message) []Message {
				return append(withSystem(s.spec.SystemPrompt, r), Message{Role: "user", Content: wrapUpInstruction})
			})
		if wrapErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finalContent, usage, nil, ctxErr
			}
			emitCapExhaustedError(ctx, hub, s.Name())
			return finalContent, usage, nil, fmt.Errorf("subagent: round cap exhausted: %w", wrapErr)
		}
		usage.Add(wrapUsage)
		s.runMu.Lock()
		s.ownUsage.Add(wrapUsage)
		s.runMu.Unlock()
		if strings.TrimSpace(wrapContent) != "" {
			finalContent = wrapContent
		} else {
			emitCapExhaustedError(ctx, hub, s.Name())
			return finalContent, usage, nil, fmt.Errorf("subagent: round cap exhausted: wrap-up round produced no content")
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(s.Name())) {
		return finalContent, usage, nil, ctx.Err()
	}
	return finalContent, usage, nil, nil
}

// executeAndRecordToolCalls dispatches calls in parallel and records every
// result: one ToolResultEvent per call plus one role="tool" message appended
// to *round. Shared by the main loop and the length-continuation path
// (llm-length-continuation, D5).
func (s *SubAgent) executeAndRecordToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub, round *[]Message, usage *Usage) {
	results := s.dispatchToolCalls(ctx, calls, hub)
	for _, tc := range calls {
		result, ok := results[tc.ID]
		if !ok {
			result = toolResult{content: "", isError: true}
		}
		if !result.isError {
			s.runMu.Lock()
			s.executedToolThisRun = true
			s.runMu.Unlock()
		}
		if result.subUsage != nil && usage != nil {
			usage.Add(*result.subUsage)
		}
		if result.subCapped && s.spec.Coordinator != nil {
			s.spec.Coordinator.meta.observeRoundCapped()
		}
		hub.SendCtx(ctx, stream.ToolResultEvent(s.Name(), tc.ID, result.content))
		*round = append(*round, Message{
			Role:       "tool",
			Content:    result.content,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
		})
	}
}

// dispatchToolCalls runs every tool_call in parallel. Depth-eligible instances
// intercept spawn_subagent; all other calls go through the narrowed registry.
func (s *SubAgent) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub) map[string]toolResult {
	results := make(map[string]toolResult, len(calls))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for _, tc := range calls {
		tc := tc
		g.Go(func() error {
			res := s.dispatchOne(gctx, tc, hub)
			mu.Lock()
			results[tc.ID] = res
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// dispatchOne routes spawn_subagent to the shared coordinator and every other
// tool_call straight to this instance's narrowed registry.
func (s *SubAgent) dispatchOne(ctx context.Context, tc ToolCall, hub stream.EventHub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(s.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}
	if IsInvokeTool(tc.Function.Name) {
		if s.spec.Coordinator == nil {
			msg := fmt.Sprintf("tool %q not available: nested dispatch is disabled", tc.Function.Name)
			streamAgentError(hub, ctx, s.Name(), msg, "unknown_tool")
			return toolResult{content: msg, isError: true}
		}
		return s.spec.Coordinator.dispatch(ctx, tc, hub, retryBudgetFromCtx(ctx), spawnParent{
			agentName:  s.Name(),
			instanceID: s.spec.InstanceID,
			depth:      s.spec.Depth,
			registry:   s.toolRegistry,
			toolNames:  s.spec.Tools,
		})
	}
	if s.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, s.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	// On a tool failure the status envelope is the sole channel — no agent_error
	// is emitted for tool failures (capability: tool-result-envelope).
	out, err := s.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
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
func (s *SubAgent) terminalResponseFormat(round []Message) (json.RawMessage, bool) {
	if len(s.responseFormat) == 0 {
		return nil, false
	}
	// No tools configured: the first (and likely only) round is terminal.
	if !s.toolsIsNotNil {
		return s.responseFormat, true
	}
	// Tooled agent: terminal only when the preceding context is a tool result
	// (we are synthesizing after a tool dispatch).
	if len(round) > 0 && round[len(round)-1].Role == "tool" {
		return s.responseFormat, true
	}
	return nil, false
}
