package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"golang.org/x/sync/errgroup"
)

// maxConfuciusRounds bounds the agent loop to prevent runaway tool-call chains
// from looping forever. 16 is generous; a healthy Confucius finishes in 1-3
// rounds.
const maxConfuciusRounds = 16

// Confucius is the central orchestrator agent. It owns its own tool-calling
// loop and is the only agent permitted to dispatch sub-agents (Chongzhi,
// Liang). Sub-agent invocation is intercepted in the dispatch switch before
// the tool registry is consulted; the synthetic invoke_chongzhi /
// invoke_liang tools therefore never reach the registry.
type Confucius struct {
	cfg           config.AgentConfig
	client        LLMClient
	toolRegistry  *tool.Registry
	subAgents     map[string]Agent // keyed by ToolInvokeChongzhi / ToolInvokeLiang
	toolsJSON     []byte           // pre-rendered OpenAI tools[] including invoke_*
	toolsIsNotNil bool
}

// NewConfucius builds a Confucius agent. subAgents maps invoke_chongzhi /
// invoke_liang to their respective Agent implementations; it must contain at
// least those keys. The tools[] JSON is rendered once at construction time
// from cfg.Tools plus the two synthetic invoke_* tools.
func NewConfucius(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, subAgents map[string]Agent) (*Confucius, error) {
	if _, ok := subAgents[ToolInvokeChongzhi]; !ok {
		return nil, fmt.Errorf("agent: confucius sub-agents missing %q", ToolInvokeChongzhi)
	}
	if _, ok := subAgents[ToolInvokeLiang]; !ok {
		return nil, fmt.Errorf("agent: confucius sub-agents missing %q", ToolInvokeLiang)
	}
	toolsJSON, err := buildConfuciusToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build confucius tools: %w", err)
	}
	return &Confucius{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		subAgents:     subAgents,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
	}, nil
}

// Name implements Agent.
func (c *Confucius) Name() string { return c.cfg.Name }

// SystemPrompt implements Agent.
func (c *Confucius) SystemPrompt() string { return c.cfg.SystemPrompt }

// RetryPolicy implements Agent. Confucius itself is never retried (it is the
// dispatcher), so it always returns a disabled policy.
func (c *Confucius) RetryPolicy() config.AgentRetryConfig { return config.AgentRetryConfig{} }

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
func (c *Confucius) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, *TurnBreakdown, error) {
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
	// retryBudget bounds the total tokens spent on sub-agent retries across the
	// whole turn (capability C). Shared across parallel dispatches; nil-safe
	// (an unset policy.BudgetTokens means unlimited).
	budget := newRetryBudget(c.cfg.Retry.BudgetTokens)

	for i := 0; i < maxConfuciusRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, buildBreakdown(byAgent, tmeta), ctx.Err()
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

		// Capture streamed tokens into assistantText as the model emits them.
		var assistantText string
		resp, err := c.client.StreamChat(ctx, req, func(delta string) error {
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

		total.Add(resp.Usage)
		byAgent[c.Name()] = addUsage(byAgent[c.Name()], resp.Usage)

		// Append the assistant turn (with any tool_calls) to the conversation
		// so the next round sees the model's reasoning + planned calls.
		assistantMsg := Message{Role: "assistant", Content: resp.Content, ReasoningContent: resp.ReasoningContent, ToolCalls: resp.ToolCalls}
		if assistantMsg.Content == "" && assistantMsg.ReasoningContent == "" && len(resp.ToolCalls) == 0 {
			// Nothing to do; treat as terminal.
			finalContent = assistantText
			break
		}
		round = append(round, assistantMsg)

		// Terminal: model finished without tool calls.
		if !shouldDispatchToolCalls(resp) {
			finalContent = resp.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		// Track parallelism: a round with >=2 tool_calls counts as parallel.
		tmeta.observeRound(resp.ToolCalls)

		// Dispatch all tool_calls in parallel. Sub-agent invocations are
		// intercepted before the registry; regular tools go through it.
		toolResults := c.dispatchToolCalls(ctx, resp.ToolCalls, hub, budget)

		// Append one role="tool" message per tool_call_id, preserving the
		// order OpenAI expects (each tool result references a tool_call_id).
		for _, tc := range resp.ToolCalls {
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
			// Record which invoke_* sub-agents fired this turn (for
			// usage.meta.sub_agent_invocations), regardless of success.
			if IsInvokeTool(tc.Function.Name) {
				tmeta.observeInvoke(tc.Function.Name)
			}
			hub.SendCtx(ctx, stream.ToolResultEvent(c.Name(), tc.ID, result.content))
			round = append(round, Message{
				Role:       "tool",
				Content:    result.content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
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
	subAgentName string  // display name of the producing sub-agent ("" for registry tools)
}

// dispatchToolCalls runs every tool_call in parallel via errgroup. Sub-agent
// invocations (invoke_chongzhi / invoke_liang) are dispatched to the matching
// Agent; everything else goes through toolRegistry.Call. Errors are streamed
// as agent_error events and turned into error-string tool results so the LLM
// can react. Returns a map keyed by tool_call.ID. budget carries the per-turn
// retry token budget shared across all parallel dispatches (concurrency-safe).
func (c *Confucius) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub *stream.Hub, budget *retryBudget) map[string]toolResult {
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

// dispatchOne resolves a single tool_call. The agent name used for error
// events is the sub-agent's own name when dispatching to a sub-agent, and
// Confucius's own name for plain tool errors (since the tool itself has no
// identity in the stream model).
func (c *Confucius) dispatchOne(ctx context.Context, tc ToolCall, hub *stream.Hub, budget *retryBudget) toolResult {
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
func (c *Confucius) dispatchSubAgent(ctx context.Context, tc ToolCall, hub *stream.Hub, budget *retryBudget) toolResult {
	sub, ok := c.subAgents[tc.Function.Name]
	if !ok {
		// Should not happen — NewConfucius validates presence — but defensive.
		msg := fmt.Sprintf("unknown sub-agent tool %q", tc.Function.Name)
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

	// First attempt.
	content, usage, _, err := sub.Run(ctx, messages, hub)
	if err == nil {
		u := usage
		return toolResult{content: content, subUsage: &u, subAgentName: sub.Name()}
	}

	// Failed. Decide retryability.
	policy := sub.RetryPolicy()
	if !shouldRetry(sub, err, policy, budget) {
		return toolResult{content: err.Error(), isError: true, subUsage: &usage, subAgentName: sub.Name()}
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
			return toolResult{content: ctx.Err().Error(), isError: true, subUsage: &usage, subAgentName: sub.Name()}
		}
		content, usage, _, err = sub.Run(ctx, messages, hub)
		if err == nil {
			u := usage
			return toolResult{content: content, subUsage: &u, subAgentName: sub.Name()}
		}
		// A non-transient follow-up error (e.g. bad_args surfaced on retry)
		// stops the loop; the last error is surfaced to Confucius.
		if !isTransientError(err) {
			break
		}
	}

	// Retries exhausted / stopped. Surface the last error to Confucius.
	return toolResult{content: err.Error(), isError: true, subUsage: &usage, subAgentName: sub.Name()}
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

func (c *Confucius) dispatchRegistryTool(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
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

// marshalToolResult renders a tool.Execute return value into the string body
// of the role="tool" message. Objects/arrays are JSON-encoded; scalars use
// fmt. nil becomes the empty string.
func marshalToolResult(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case error:
		return x.Error()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// streamAgentError is a best-effort agent_error emission. It does not block
// on a closed hub or cancelled context — losing an error event during
// teardown is acceptable.
func streamAgentError(hub *stream.Hub, ctx context.Context, agent, msg, code string) {
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
	invokes, parallel := tmeta.snapshot()
	out := make(map[string]Usage, len(byAgent))
	for k, v := range byAgent {
		out[k] = v
	}
	return &TurnBreakdown{
		ByAgent:             out,
		Parallel:            parallel,
		SubAgentInvocations: invokes,
	}
}

// turnMeta accumulates cross-agent facts about one Confucius turn for the done
// event's usage.meta: which invoke_* sub-agents fired (sub_agent_invocations,
// in dispatch order, deduplicated) and whether any assistant round dispatched
// >=2 tool_calls (parallel). It is concurrency-safe because dispatch happens
// across goroutines; observeRound/observeInvoke may be called from multiple
// goroutines.
type turnMeta struct {
	mu            sync.Mutex
	parallel      bool
	invokes       []string // ordered, deduplicated invoke_* tool names
	invokesSeen   map[string]struct{}
}

func newTurnMeta() *turnMeta {
	return &turnMeta{invokesSeen: map[string]struct{}{}}
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

// snapshot returns the invoke list and parallel flag safe for emission. The
// returned slice is a copy so callers may use it after further mutations.
func (t *turnMeta) snapshot() ([]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.invokes))
	copy(out, t.invokes)
	return out, t.parallel
}
