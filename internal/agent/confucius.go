package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"golang.org/x/sync/errgroup"
)

// Confucius is the central orchestrator agent. It owns its own tool-calling
// loop and is the only agent permitted to dispatch sub-agents (Chongzhi,
// Liang). Sub-agent invocation is intercepted in the dispatch switch before
// the tool registry is consulted; the synthetic invoke_chongzhi /
// invoke_liang tools therefore never reach the registry.
type Confucius struct {
	cfg          config.AgentConfig
	client       LLMClient
	toolRegistry *tool.Registry
	// turn is the turn-level model/effort configuration (model-effort-v2):
	// the handler-resolved catalog entry + effort injected at construction —
	// agents no longer carry model fields of their own. Read-only after
	// construction, so per-invocation isolation is preserved by the factory
	// building a fresh agent per dispatch.
	turn ModelOverride
	// subAgents maps ToolInvokeChongzhi / ToolInvokeLiang to per-invocation
	// factories (not shared instances): every dispatch builds a fresh sub-agent
	// so concurrent same-name invocations never share mutable run state
	// (executedToolThisRun / hitCapThisRun; subagent-run-identity capability).
	subAgents     map[string]SubAgentFactory
	toolsJSON     []byte // pre-rendered OpenAI tools[] including invoke_*
	toolsIsNotNil bool
	// maxRounds bounds the tool-calling loop; resolved from cfg.MaxRounds
	// (default config.DefaultAgentMaxRounds) at construction.
	maxRounds int
	// hitCapThisRun records whether the most recent Run exited via the cap
	// path; exposed via LastRunHitCap. Confucius is never consulted as a
	// sub-agent, but it implements RoundCapTracker for uniformity with the
	// leaf agents. Reset at the top of each Run.
	hitCapThisRun bool
	// roundHook is the optional between-rounds seam (context-compaction
	// mid-turn trigger). Nil on leaf-agent-free runs unless the orchestrator
	// installed one right after the factory built this agent; see roundhook.go.
	roundHook RoundHook
}

// SetRoundHook implements RoundHookSetter. The orchestrator calls it on the
// freshly-built per-request Confucius so the hook (and everything it captures)
// never outlives the turn.
func (c *Confucius) SetRoundHook(hook RoundHook) { c.roundHook = hook }

// NewConfucius builds a Confucius agent. subAgents maps invoke_chongzhi /
// invoke_liang to SubAgentFactories; it must contain at least those keys. Each
// factory is invoked once per dispatch to build a per-invocation instance (see
// SubAgentFactory). The tools[] JSON is rendered once at construction time
// from cfg.Tools plus the two synthetic invoke_* tools. turn is the
// turn-resolved model/effort/quota/continuation configuration applied to every
// LLM call this agent makes (model-effort-v2, per-model-completion-budget) —
// including the write-budget number injected into the tools[] descriptions
// and the turn's finish_reason=length continuation policy.
func NewConfucius(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, subAgents map[string]SubAgentFactory, turn ModelOverride) (*Confucius, error) {
	if _, ok := subAgents[ToolInvokeChongzhi]; !ok {
		return nil, fmt.Errorf("agent: confucius sub-agents missing %q", ToolInvokeChongzhi)
	}
	if _, ok := subAgents[ToolInvokeLiang]; !ok {
		return nil, fmt.Errorf("agent: confucius sub-agents missing %q", ToolInvokeLiang)
	}
	toolsJSON, err := buildConfuciusToolsJSON(reg, cfg.Tools, turn.MaxCompletionTokens)
	if err != nil {
		return nil, fmt.Errorf("agent: build confucius tools: %w", err)
	}
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.DefaultAgentMaxRounds()
	}
	return &Confucius{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		turn:          turn,
		subAgents:     subAgents,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
		maxRounds:     maxRounds,
	}, nil
}

// Name implements Agent.
func (c *Confucius) Name() string { return c.cfg.Name }

// SystemPrompt implements Agent.
func (c *Confucius) SystemPrompt() string { return c.cfg.SystemPrompt }

// RetryPolicy implements Agent. The dispatch-level retry pipeline never
// consults it (Confucius is the dispatcher and is itself never dispatched), so
// it stays a disabled policy — its interface semantics are unchanged.
// Confucius's ROUND-level transient retry (llm-round-retry) instead reads
// c.cfg.Retry directly in its own loop.
func (c *Confucius) RetryPolicy() config.AgentRetryConfig { return config.AgentRetryConfig{} }

// LastRunHitCap implements RoundCapTracker (uniformity; Confucius is never
// consulted as a sub-agent).
func (c *Confucius) LastRunHitCap() bool { return c.hitCapThisRun }

// Run executes the Confucius agent loop. It streams lifecycle events to hub
// and returns the final assistant content + aggregated usage + the per-agent
// usage breakdown. The loop terminates when:
//   - the model returns finish_reason="stop" (or no tool_calls and no content),
//   - ctx is cancelled,
//   - maxConfuciusRounds is exceeded (defensive guard against infinite loops).
//
// On sub-agent or tool failure, an agent_error event is streamed and the
// error text is fed back to the model as the tool result so the LLM can react.
//
// The returned TurnBreakdown is the per-agent cost attribution: it always
// contains Confucius's own usage under "Confucius", plus one entry per
// dispatched sub-agent under its display name, and the turn-level meta
// (parallel flag + ordered invoke_* list). emitDone renders it as
// usage.by_agent / usage.meta on the done event and it is persisted verbatim
// into turn_usage.usage_json.
func (c *Confucius) Run(ctx context.Context, messages []Message, hub stream.EventHub) (string, Usage, *TurnBreakdown, error) {
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

	// byAgent carries per-agent usage; Confucius accumulates its own usage
	// under the "Confucius" key, and each dispatched sub-agent under its own
	// name. Used to render usage.by_agent on the done event.
	byAgent := map[string]Usage{}
	// turnMeta tracks cross-agent turn facts: which invoke_* tools fired and
	// whether any assistant round dispatched >=2 tool_calls in parallel.
	tmeta := newTurnMeta()
	// retryBudget bounds the total tokens spent on retries across the whole
	// turn (capability C; llm-round-retry widens it to cover round-level
	// retries too — one shared pool per turn). It is injected into ctx so
	// runLLMRound's round-level retries (this loop's own AND every dispatched
	// sub-agent's, whose contexts derive from this one) charge the same pool
	// the dispatch-level retry below reads. Shared across parallel dispatches;
	// nil-safe (an unset policy.BudgetTokens means unlimited).
	budget := newRetryBudget(c.cfg.Retry.BudgetTokens)
	ctx = WithRetryBudget(ctx, budget)
	c.hitCapThisRun = false
	var capped bool // set when the loop exits by hitting the round cap (not via a natural break)

	for i := 0; i < c.maxRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, buildBreakdown(byAgent, tmeta), ctx.Err()
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

		// Capture streamed tokens into assistantText as the model emits them
		// (across ALL attempts of the round — the continuation stream is
		// seamless, so the fallback accumulates too).
		var assistantText string
		result, err := runLLMRound(ctx, c.client, hub, c.Name(), req, &round, c.turn.LengthContinue, c.cfg.Retry,
			func(r []Message) []Message { return withSystem(c.cfg.SystemPrompt, r) },
			// Dispatch of the parseable tool_calls of an intermediate length
			// response (design D5): reuses the main loop's
			// dispatch-and-record block so sub-agent usage folding, cap
			// propagation, and invoke_* tracking apply unchanged.
			func(gctx context.Context, calls []ToolCall) {
				c.executeAndRecordToolCalls(gctx, calls, hub, budget, tmeta, &total, byAgent, &round)
			},
			func(delta string) error {
				assistantText += delta
				// SendCtx returns false on ctx cancel or hub close; we surface
				// the cancel so StreamChat aborts.
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
				return finalContent, total, buildBreakdown(byAgent, tmeta), ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return finalContent, total, buildBreakdown(byAgent, tmeta), fmt.Errorf("confucius: stream chat: %w", err)
		}

		total.Add(result.Usage)
		byAgent[c.Name()] = addUsage(byAgent[c.Name()], result.Usage)
		// Record the end-of-round context size (the FINAL attempt's
		// authoritative prompt+completion — never the cross-attempt sum, which
		// would inflate the next-request size; design D8) so the done event's
		// usage.meta.context_tokens — and through it turn_usage.context_tokens —
		// carries the turn's LAST round for the next turn's preventive
		// compaction check.
		tmeta.observeRoundContext(result.Resp.Usage.PromptTokens + result.Resp.Usage.CompletionTokens)

		// llm-length-continuation exhaustion: every attempt ended length. The
		// turn fails loudly (agent_error + done.error) but keeps the
		// accumulated partial content as finalContent — continuation never
		// discards. Mirrors the round_cap_exhausted shape.
		if c.turn.LengthContinue.Enabled() && result.LengthHit {
			step, retries := c.turn.LengthContinue.Resolve()
			msg := lengthExhaustedMessage(c.Name(), retries, c.turn.MaxCompletionTokens+retries*step)
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), msg, "length_exhausted"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return result.Content, total, buildBreakdown(byAgent, tmeta), fmt.Errorf("confucius: %s", msg)
		}

		resp := result.Resp

		// Append the assistant turn (with any tool_calls) to the conversation
		// so the next round sees the model's reasoning + planned calls.
		assistantMsg := Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && assistantMsg.ReasoningContent == "" && len(resp.ToolCalls) == 0 {
			// Nothing to do; treat as terminal.
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		// Terminal: model finished without tool calls. finalContent is the
		// content accumulated across the round's attempts (continuation keeps
		// partial output), falling back to the streamed accumulation when the
		// client leaves Content empty.
		if !shouldDispatchToolCalls(resp) {
			finalContent = result.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		// Track parallelism: a round with >=2 tool_calls counts as parallel.
		tmeta.observeRound(resp.ToolCalls)

		// Dispatch all tool_calls in parallel. Sub-agent invocations are
		// intercepted before the registry; regular tools go through it.
		c.executeAndRecordToolCalls(ctx, resp.ToolCalls, hub, budget, tmeta, &total, byAgent, &round)

		// Between-rounds context-pressure seam (context-compaction, mid-turn
		// trigger): after this round's tool results are appended and before
		// the next LLM request, hand the round's authoritative usage to the
		// installed hook. It may rewrite `round` to the compacted context
		// (first message + framed summary + retained tail); nil keeps the
		// conversation unchanged. The hook is silent on the SSE stream by
		// contract and never aborts the loop.
		if c.roundHook != nil {
			if replaced := c.roundHook(ctx, resp.Usage); replaced != nil {
				round = replaced
			}
		}

		// This iteration dispatched tools and did not break; flag a cap exit
		// when it was the last allowed round so the post-loop wrap-up runs.
		capped = i+1 == c.maxRounds
	}

	// Round cap reached without a natural stop: give the model one
	// tool-disabled wrap-up round to synthesize a final answer instead of
	// ending empty. Emit the always-on WARN log + meta.round_capped; the
	// user-facing agent_error fires only if the wrap-up recovers no content.
	if capped {
		c.hitCapThisRun = true
		emitCapHitWarn(c.Name(), c.maxRounds, c.maxRounds)
		tmeta.observeRoundCapped()
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
				return finalContent, total, buildBreakdown(byAgent, tmeta), ctxErr
			}
			emitCapExhaustedError(ctx, hub, c.Name())
			return finalContent, total, buildBreakdown(byAgent, tmeta), fmt.Errorf("confucius: round cap exhausted: %w", wrapErr)
		}
		total.Add(wrapUsage)
		byAgent[c.Name()] = addUsage(byAgent[c.Name()], wrapUsage)
		// The wrap-up round is the turn's last LLM interaction; its
		// prompt+completion is the closest measurement of the context the
		// session ends on.
		tmeta.observeRoundContext(wrapUsage.PromptTokens + wrapUsage.CompletionTokens)
		if strings.TrimSpace(wrapContent) != "" {
			finalContent = wrapContent
		} else {
			// Wrap-up produced no content: genuine empty-end. Emit the loud
			// signal and return a non-nil error so the orchestrator's done
			// event carries `error` (spec: "Cap hit with empty wrap-up round
			// surfaces error"). emitCapExhaustedError already emitted
			// agent_end, so return directly (skip the normal agent_end below).
			emitCapExhaustedError(ctx, hub, c.Name())
			return finalContent, total, buildBreakdown(byAgent, tmeta), fmt.Errorf("confucius: round cap exhausted: wrap-up round produced no content")
		}
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(c.Name())) {
		return finalContent, total, buildBreakdown(byAgent, tmeta), ctx.Err()
	}
	return finalContent, total, buildBreakdown(byAgent, tmeta), nil
}

// toolResult is the resolved outcome of one tool_call. content is fed back to
// the model verbatim as the role="tool" message body; isError is currently
// informational (the content already contains the error message). subUsage is
// non-nil when this result came from a sub-agent run; subAgentName then names
// that sub-agent so the parent can attribute cost to its per-agent key.
type toolResult struct {
	content      string
	isError      bool
	subUsage     *Usage // non-nil when this result came from a sub-agent run
	subAgentName string // display name of the producing sub-agent ("" for registry tools)
	subCapped    bool   // true when the producing sub-agent hit its max_rounds cap this Run
}

// dispatchToolCalls runs every tool_call in parallel via errgroup. Sub-agent
// invocations (invoke_chongzhi / invoke_liang) are dispatched to the matching
// Agent; everything else goes through toolRegistry.Call. Errors are streamed
// as agent_error events and turned into error-string tool results so the LLM
// can react. Returns a map keyed by tool_call.ID. budget carries the per-turn
// retry token budget shared across all parallel dispatches (concurrency-safe).
func (c *Confucius) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub, budget *retryBudget) map[string]toolResult {
	results := make(map[string]toolResult, len(calls))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	for _, tc := range calls {
		tc := tc // capture for goroutine
		g.Go(func() error {
			// errgroup cancels gctx on first non-nil return; we never want
			// one tool failure to abort the others, so we always return nil
			// and surface failures through the result map + events.
			res := c.dispatchOne(gctx, tc, hub, budget)
			mu.Lock()
			results[tc.ID] = res
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// executeAndRecordToolCalls dispatches calls in parallel via dispatchToolCalls
// and folds every result into the turn accounting and the round: sub-agent
// token usage into total/byAgent, cap propagation and invoke_* tracking into
// tmeta, one ToolResultEvent per call, and one role="tool" message per call
// appended to *round (each tool result references its tool_call_id, in the
// order OpenAI expects). Shared by the main loop and the length-continuation
// path — the latter dispatches the parseable calls of an intermediate length
// response before the continuation request (llm-length-continuation, D5).
func (c *Confucius) executeAndRecordToolCalls(ctx context.Context, calls []ToolCall, hub stream.EventHub, budget *retryBudget, tmeta *turnMeta, total *Usage, byAgent map[string]Usage, round *[]Message) {
	toolResults := c.dispatchToolCalls(ctx, calls, hub, budget)
	for _, tc := range calls {
		result, ok := toolResults[tc.ID]
		if !ok {
			result = toolResult{content: "", isError: true}
		}
		// Fold sub-agent token usage into the turn total so the done
		// event reports the full cost including dispatched sub-agents.
		// Registry tools incur no LLM usage, so subUsage is nil for them.
		if result.subUsage != nil {
			total.Add(*result.subUsage)
			// Attribute the sub-agent's cost to its own key in the
			// per-agent breakdown (preserved instead of being folded into
			// the Confucius total). The sub-agent's display name is
			// recorded on the result by dispatchSubAgent.
			if result.subAgentName != "" {
				byAgent[result.subAgentName] = addUsage(byAgent[result.subAgentName], *result.subUsage)
			}
		}
		// Propagate a sub-agent cap into the turn-level meta: a capped
		// sub-agent marks usage.meta.round_capped even if Confucius itself
		// never hit its own cap.
		if result.subCapped {
			tmeta.observeRoundCapped()
		}
		// Record which invoke_* sub-agents fired this turn (for
		// usage.meta.sub_agent_invocations), regardless of success.
		if IsInvokeTool(tc.Function.Name) {
			tmeta.observeInvoke(tc.Function.Name)
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

// dispatchOne resolves a single tool_call. The agent name used for error
// events is the sub-agent's own name when dispatching to a sub-agent, and
// Confucius's own name for plain tool errors (since the tool itself has no
// identity in the stream model).
func (c *Confucius) dispatchOne(ctx context.Context, tc ToolCall, hub stream.EventHub, budget *retryBudget) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(c.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}

	if IsInvokeTool(tc.Function.Name) {
		return c.dispatchSubAgent(ctx, tc, hub, budget)
	}
	return c.dispatchRegistryTool(ctx, tc, hub)
}

// dispatchSubAgent runs the named sub-agent with an isolated context: only
// the sub-agent's own system prompt + one user message assembled from the
// invoke tool's {task, context} arguments. The sub-agent's events propagate
// up through the shared hub (already wired — same hub, the sub-agent's Run
// emits its own agent_start/token/agent_end). Its usage is folded back into
// the result so Confucius can aggregate.
//
// Capability C — transient error retry: when the sub-agent's RetryPolicy is
// enabled and the failure is transient (429/5xx/timeout), the LLM call is
// retried up to MaxAttempts with exponential backoff (capped at MaxBackoff),
// subject to the per-turn token budget (budget). Semantic errors (bad_args,
// unknown_tool) are never retried; side-effecting agents (those implementing
// ToolCallTracker) are retried only before they execute any tool call. Each
// retry emits an agent_error event with Meta.retry=true so the frontend can
// signal the retry. Retries are skipped entirely when the policy is disabled
// (Chongzhi by default) or the budget is exhausted.
func (c *Confucius) dispatchSubAgent(ctx context.Context, tc ToolCall, hub stream.EventHub, budget *retryBudget) toolResult {
	factory, ok := c.subAgents[tc.Function.Name]
	if !ok {
		// Should not happen — NewConfucius validates presence — but defensive.
		msg := fmt.Sprintf("unknown sub-agent tool %q", tc.Function.Name)
		streamAgentError(hub, ctx, subAgentNameFor(tc.Function.Name), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}

	// Build a FRESH instance for this invocation and discard it afterwards.
	// The per-run flags the dispatcher reads afterwards (ToolCallTracker's
	// side-effect flag, RoundCapTracker's cap flag) live on the instance, so
	// per-invocation construction makes them per-invocation by construction:
	// a concurrent same-name invocation can neither reset nor set them for
	// this call. Retries below reuse the SAME instance (one logical call).
	sub, err := factory()
	if err != nil {
		// A construction failure means the sub-agent is effectively
		// unavailable for this dispatch (config/wiring error); it is
		// deterministic, so surface it without retry, mirroring the
		// unknown-tool path.
		msg := fmt.Sprintf("build sub-agent for %q: %v", tc.Function.Name, err)
		streamAgentError(hub, ctx, subAgentNameFor(tc.Function.Name), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}

	var args InvokeToolArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		msg := fmt.Sprintf("parse %s args: %v", tc.Function.Name, err)
		streamAgentError(hub, ctx, sub.Name(), msg, "bad_args")
		return toolResult{content: msg, isError: true}
	}
	if args.Task == "" {
		msg := fmt.Sprintf("%s: missing required field %q", tc.Function.Name, "task")
		streamAgentError(hub, ctx, sub.Name(), msg, "bad_args")
		return toolResult{content: msg, isError: true}
	}

	userMsg := buildSubAgentUserMessage(args)
	messages := []Message{{Role: "user", Content: userMsg}}

	// Stamp every event this Run emits with the invocation's identity: the
	// invoke tool_call id. The tagging view is producer-only — Confucius's own
	// dispatcher-emitted events (the invoke tool_call/tool_result pair, retry
	// agent_error) keep flowing through the raw hub untagged.
	runHub := stream.TaggedWithRunID(hub, tc.ID)

	// First attempt.
	content, usage, _, err := sub.Run(ctx, messages, runHub)
	if err == nil {
		u := usage
		return toolResult{content: content, subUsage: &u, subAgentName: sub.Name(), subCapped: subHitCap(sub)}
	}

	// Failed. Decide retryability.
	policy := sub.RetryPolicy()
	if !shouldRetry(sub, err, policy, budget) {
		return toolResult{content: joinFailure(err, content), isError: true, subUsage: &usage, subAgentName: sub.Name(), subCapped: subHitCap(sub)}
	}

	// Retry loop: attempts are numbered from 1 (the first RETRY). MaxAttempts
	// is the total attempt count including the initial call already performed.
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = config.DefaultRetryMaxAttempts()
	}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		// Charge the failed attempt's cost to the budget before deciding to
		// retry, so a flapping downstream cannot burn unbounded tokens.
		budget.charge(usage)
		if !budget.allows() {
			break
		}
		// Idempotency re-check before each retry: a prior attempt may have
		// executed a tool call between this check and the failure.
		if tracker, ok := sub.(ToolCallTracker); ok && tracker.LastRunExecutedTool() {
			break
		}
		// Signal the retry to the frontend: an agent_error carrying Meta.retry=true
		// (plus the triggering error) so the UI can distinguish a retry from a
		// terminal failure.
		hub.SendCtx(ctx, retryErrorEvent(sub.Name(), err))
		select {
		case <-time.After(computeBackoff(policy, attempt)):
		case <-ctx.Done():
			return toolResult{content: joinFailure(ctx.Err(), content), isError: true, subUsage: &usage, subAgentName: sub.Name()}
		}
		content, usage, _, err = sub.Run(ctx, messages, runHub)
		if err == nil {
			u := usage
			return toolResult{content: content, subUsage: &u, subAgentName: sub.Name(), subCapped: subHitCap(sub)}
		}
		// A non-transient follow-up error (e.g. bad_args surfaced on retry)
		// stops the loop; the last error is surfaced to Confucius.
		if !isTransientError(err) {
			break
		}
	}

	// Retries exhausted / stopped. Surface the last error to Confucius.
	// content holds the LAST attempt's partial output — every sub.Run above
	// overwrites it, matching the one-logical-call retry semantics (earlier
	// attempts' partials are not kept).
	return toolResult{content: joinFailure(err, content), isError: true, subUsage: &usage, subAgentName: sub.Name(), subCapped: subHitCap(sub)}
}

// joinFailure merges a sub-agent's give-up error with the partial output its
// Run returned alongside the error (subagent-partial-output-on-failure). A
// blank partial keeps the tool result content byte-identical to the legacy
// error-only text (zero behavior change when nothing was salvaged — e.g. a
// round_cap_exhausted failure); otherwise the error text comes first,
// separated by a fixed marker line, so the model sees both the failure fact
// and the work already produced.
func joinFailure(err error, partial string) string {
	if strings.TrimSpace(partial) == "" {
		return err.Error()
	}
	return err.Error() + "\n\n--- partial output before failure ---\n\n" + partial
}

// shouldRetry reports whether a failed sub-agent dispatch should be retried:
// the policy must be enabled, the error must be transient, the agent must not
// have already executed a side-effecting tool call, and the budget must allow
// at least one more attempt. It is the single retryability decision point.
func shouldRetry(sub Agent, err error, policy config.AgentRetryConfig, budget *retryBudget) bool {
	if !policy.Enabled {
		return false
	}
	if !isTransientError(err) {
		return false
	}
	// Side-effecting agents are retried only before any tool call executed.
	if tracker, ok := sub.(ToolCallTracker); ok && tracker.LastRunExecutedTool() {
		return false
	}
	return budget.allows()
}

// subHitCap reports whether the sub-agent's most recent Run hit its
// max_rounds cap, so the dispatcher can propagate "a sub-agent was capped"
// into the turn-level usage.meta.round_capped. Agents without the
// RoundCapTracker capability are treated as never capped.
func subHitCap(sub Agent) bool {
	if t, ok := sub.(RoundCapTracker); ok {
		return t.LastRunHitCap()
	}
	return false
}

func (c *Confucius) dispatchRegistryTool(ctx context.Context, tc ToolCall, hub stream.EventHub) toolResult {
	if c.toolRegistry == nil {
		msg := fmt.Sprintf("tool %q not available: no tool registry", tc.Function.Name)
		streamAgentError(hub, ctx, c.Name(), msg, "unknown_tool")
		return toolResult{content: msg, isError: true}
	}
	out, err := c.toolRegistry.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	// On a tool failure the status envelope (renderToolResult's
	// {"status":1,"error":...}) is the sole channel: the model sees the error
	// in-band in the role="tool" message and the frontend renders it from the
	// tool_result status field. No agent_error is emitted for tool failures —
	// that would misrepresent a recoverable tool hiccup as an agent failure and
	// double-display on reload (capability: tool-result-envelope).
	return toolResult{content: renderToolResult(out, err), isError: err != nil}
}

// withSystem prepends a system message to msgs iff prompt is non-empty.
func withSystem(prompt string, msgs []Message) []Message {
	if prompt == "" {
		return msgs
	}
	return append([]Message{{Role: "system", Content: prompt}}, msgs...)
}

// buildSubAgentUserMessage assembles the single user message handed to a
// sub-agent from the invoke tool's {task, context} arguments.
func buildSubAgentUserMessage(args InvokeToolArgs) string {
	if args.Context == "" {
		return args.Task
	}
	return fmt.Sprintf("Task: %s\n\nContext:\n%s", args.Task, args.Context)
}

// toolEnvelope is the uniform shape every registry-tool result is rendered
// into before becoming the role="tool" message body. Field order is fixed
// (status, result, error) so the rendered JSON is stable; omitempty drops
// result on failure and error on success.
type toolEnvelope struct {
	Status int             `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// renderToolResult renders a tool.Execute return value and its error into the
// uniform status envelope that becomes the role="tool" message body:
//
//	success → {"status":0,"result":<JSON-encoded out>}
//	failure → {"status":1,"error":"<err.Error()>"}
//
// It is the single rendering point shared by every agent's registry-tool
// dispatch path (Confucius, Chongzhi, Liang). On success a []byte return is
// normalized to a Go string before encoding so it is emitted as text rather
// than base64 (matching the prior marshalToolResult semantics); nil renders as
// result:null. (json.RawMessage is a distinct named type from []byte, so it is
// NOT normalized here — it marshals natively to its raw bytes.) On failure the
// envelope carries the error message and no result field; the caller still
// emits the independent agent_error SSE event for the frontend.
func renderToolResult(out any, err error) string {
	if err != nil {
		b, _ := json.Marshal(toolEnvelope{Status: 1, Error: err.Error()})
		return string(b)
	}
	// Normalize a plain []byte return to a string so it JSON-encodes as text,
	// not base64. json.RawMessage (a named []byte type) is excluded by Go's
	// type switch and left to marshal as raw JSON.
	if b, ok := out.([]byte); ok {
		out = string(b)
	}
	raw, mErr := json.Marshal(out)
	if mErr != nil {
		// A non-serializable return is itself a tool failure: surface it via the
		// error branch rather than emitting malformed JSON.
		b, _ := json.Marshal(toolEnvelope{Status: 1, Error: fmt.Sprintf("render tool result: %v", mErr)})
		return string(b)
	}
	b, _ := json.Marshal(toolEnvelope{Status: 0, Result: raw})
	return string(b)
}

// streamAgentError is a best-effort agent_error emission. It does not block
// on a closed hub or cancelled context — losing an error event during
// teardown is acceptable.
func streamAgentError(hub stream.EventHub, ctx context.Context, agent, msg, code string) {
	hub.SendCtx(ctx, stream.AgentErrorEvent(agent, msg, code))
}

// subAgentNameFor returns the agent display name an invoke_* tool dispatches
// to. Used only for the defensive "unknown tool" path where we don't have a
// sub-agent instance to ask for its Name().
func subAgentNameFor(toolName string) string {
	switch toolName {
	case ToolInvokeChongzhi:
		return "Chongzhi"
	case ToolInvokeLiang:
		return "Liang"
	}
	return toolName
}

// addUsage returns base with delta merged in, returning a new map value so it
// is safe to call when base is the zero value. Mirrors Usage.Add without the
// pointer receiver so it composes with map lookups.
func addUsage(base, delta Usage) Usage {
	base.Add(delta)
	return base
}

// buildBreakdown freezes the in-flight byAgent map + turnMeta into the
// immutable *TurnBreakdown returned by Confucius.Run. byAgent is copied so
// callers cannot mutate the live map; the invoke list is snapshotted via
// tmeta.snapshot. Safe to call on every return path (including error paths)
// so partial attribution is never lost.
func buildBreakdown(byAgent map[string]Usage, tmeta *turnMeta) *TurnBreakdown {
	invokes, parallel, roundCapped, lastRoundContext := tmeta.snapshot()
	out := make(map[string]Usage, len(byAgent))
	for k, v := range byAgent {
		out[k] = v
	}
	return &TurnBreakdown{
		ByAgent:                out,
		Parallel:               parallel,
		SubAgentInvocations:    invokes,
		RoundCapped:            roundCapped,
		LastRoundContextTokens: lastRoundContext,
	}
}

// turnMeta accumulates cross-agent facts about one Confucius turn for the done
// event's usage.meta: which invoke_* sub-agents fired (sub_agent_invocations,
// in dispatch order, deduplicated) and whether any assistant round dispatched
// >=2 tool_calls (parallel). It is concurrency-safe because dispatch happens
// across goroutines; observeRound/observeInvoke may be called from multiple
// goroutines.
type turnMeta struct {
	mu          sync.Mutex
	parallel    bool
	roundCapped bool
	invokes     []string // ordered, deduplicated invoke_* tool names
	invokesSeen map[string]struct{}
	// lastRoundContext is the most recent LLM round's prompt+completion —
	// the authoritative end-of-turn context size (context-compaction
	// capability), rendered into usage.meta.context_tokens.
	lastRoundContext int
}

func newTurnMeta() *turnMeta {
	return &turnMeta{invokesSeen: map[string]struct{}{}}
}

// observeRoundContext records the context size of the LLM round that just
// completed, overwriting any earlier round (the LAST round's value is what
// turn_usage.context_tokens needs — never the cumulative sum).
func (t *turnMeta) observeRoundContext(tokens int) {
	t.mu.Lock()
	t.lastRoundContext = tokens
	t.mu.Unlock()
}

// observeRound records whether one assistant round's tool_calls constitute a
// parallel dispatch (>=2 calls in the same round).
func (t *turnMeta) observeRound(calls []ToolCall) {
	if len(calls) >= 2 {
		t.mu.Lock()
		t.parallel = true
		t.mu.Unlock()
	}
}

// observeRoundCapped marks the turn as having seen at least one agent hit its
// max_rounds cap (Confucius itself or any dispatched sub-agent), so the done
// event's usage.meta.round_capped reflects it. Safe under concurrent dispatch.
func (t *turnMeta) observeRoundCapped() {
	t.mu.Lock()
	t.roundCapped = true
	t.mu.Unlock()
}

// observeInvoke records that the named invoke_* sub-agent was dispatched this
// turn, preserving first-seen order and de-duplicating.
func (t *turnMeta) observeInvoke(toolName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.invokesSeen[toolName]; ok {
		return
	}
	t.invokesSeen[toolName] = struct{}{}
	t.invokes = append(t.invokes, toolName)
}

// snapshot returns the invoke list, parallel flag, round-capped flag, and
// last-round context size safe for emission. The returned slice is a copy so
// callers may use it after further mutations.
func (t *turnMeta) snapshot() ([]string, bool, bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.invokes))
	copy(out, t.invokes)
	return out, t.parallel, t.roundCapped, t.lastRoundContext
}
