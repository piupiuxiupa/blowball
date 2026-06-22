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

// maxConfuseRounds bounds the agent loop to prevent runaway tool-call chains
// from looping forever. 16 is generous; a healthy Confuse finishes in 1-3
// rounds.
const maxConfuseRounds = 16

// Confuse is the central orchestrator agent. It owns its own tool-calling
// loop and is the only agent permitted to dispatch sub-agents (Chongzhi,
// Liang). Sub-agent invocation is intercepted in the dispatch switch before
// the tool registry is consulted; the synthetic invoke_chongzhi /
// invoke_liang tools therefore never reach the registry.
type Confuse struct {
	cfg           config.AgentConfig
	client        LLMClient
	toolRegistry  *tool.Registry
	subAgents     map[string]Agent // keyed by ToolInvokeChongzhi / ToolInvokeLiang
	toolsJSON     []byte           // pre-rendered OpenAI tools[] including invoke_*
	toolsIsNotNil bool
}

// NewConfuse builds a Confuse agent. subAgents maps invoke_chongzhi /
// invoke_liang to their respective Agent implementations; it must contain at
// least those keys. The tools[] JSON is rendered once at construction time
// from cfg.Tools plus the two synthetic invoke_* tools.
func NewConfuse(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, subAgents map[string]Agent) (*Confuse, error) {
	if _, ok := subAgents[ToolInvokeChongzhi]; !ok {
		return nil, fmt.Errorf("agent: confuse sub-agents missing %q", ToolInvokeChongzhi)
	}
	if _, ok := subAgents[ToolInvokeLiang]; !ok {
		return nil, fmt.Errorf("agent: confuse sub-agents missing %q", ToolInvokeLiang)
	}
	toolsJSON, err := buildConfuseToolsJSON(reg, cfg.Tools)
	if err != nil {
		return nil, fmt.Errorf("agent: build confuse tools: %w", err)
	}
	return &Confuse{
		cfg:           cfg,
		client:        client,
		toolRegistry:  reg,
		subAgents:     subAgents,
		toolsJSON:     toolsJSON,
		toolsIsNotNil: len(toolsJSON) > 0 && string(toolsJSON) != "null",
	}, nil
}

// Name implements Agent.
func (c *Confuse) Name() string { return c.cfg.Name }

// SystemPrompt implements Agent.
func (c *Confuse) SystemPrompt() string { return c.cfg.SystemPrompt }

// Run executes the Confuse agent loop. It streams lifecycle events to hub
// and returns the final assistant content + aggregated usage. The loop
// terminates when:
//   - the model returns finish_reason="stop" (or no tool_calls and no content),
//   - ctx is cancelled,
//   - maxConfuseRounds is exceeded (defensive guard against infinite loops).
//
// On sub-agent or tool failure, an agent_error event is streamed and the
// error text is fed back to the model as the tool result so the LLM can react.
func (c *Confuse) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, error) {
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

	for i := 0; i < maxConfuseRounds; i++ {
		select {
		case <-ctx.Done():
			return finalContent, total, ctx.Err()
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
				return finalContent, total, ctxErr
			}
			hub.SendCtx(ctx, stream.AgentErrorEvent(c.Name(), err.Error(), "llm_error"))
			hub.SendCtx(ctx, stream.AgentEndEvent(c.Name()))
			return finalContent, total, fmt.Errorf("confuse: stream chat: %w", err)
		}

		total.Add(resp.Usage)

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
		if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) == 0 {
			finalContent = resp.Content
			if finalContent == "" {
				finalContent = assistantText
			}
			break
		}

		// Dispatch all tool_calls in parallel. Sub-agent invocations are
		// intercepted before the registry; regular tools go through it.
		toolResults := c.dispatchToolCalls(ctx, resp.ToolCalls, hub)

		// Append one role="tool" message per tool_call_id, preserving the
		// order OpenAI expects (each tool result references a tool_call_id).
		for _, tc := range resp.ToolCalls {
			result, ok := toolResults[tc.ID]
			if !ok {
				result = toolResult{content: "", isError: true}
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
		return finalContent, total, ctx.Err()
	}
	return finalContent, total, nil
}

// toolResult is the resolved outcome of one tool_call. content is fed back to
// the model verbatim as the role="tool" message body; isError is currently
// informational (the content already contains the error message).
type toolResult struct {
	content  string
	isError  bool
	subUsage *Usage // non-nil when this result came from a sub-agent run
}

// dispatchToolCalls runs every tool_call in parallel via errgroup. Sub-agent
// invocations (invoke_chongzhi / invoke_liang) are dispatched to the matching
// Agent; everything else goes through toolRegistry.Call. Errors are streamed
// as agent_error events and turned into error-string tool results so the LLM
// can react. Returns a map keyed by tool_call.ID.
func (c *Confuse) dispatchToolCalls(ctx context.Context, calls []ToolCall, hub *stream.Hub) map[string]toolResult {
	results := make(map[string]toolResult, len(calls))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	for _, tc := range calls {
		tc := tc // capture for goroutine
		g.Go(func() error {
			// errgroup cancels gctx on first non-nil return; we never want
			// one tool failure to abort the others, so we always return nil
			// and surface failures through the result map + events.
			res := c.dispatchOne(gctx, tc, hub)
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
// Confuse's own name for plain tool errors (since the tool itself has no
// identity in the stream model).
func (c *Confuse) dispatchOne(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
	if !hub.SendCtx(ctx, stream.ToolCallEvent(c.Name(), tc.ID, tc.Function.Name, json.RawMessage(tc.Function.Arguments))) {
		return toolResult{content: "", isError: true}
	}

	if IsInvokeTool(tc.Function.Name) {
		return c.dispatchSubAgent(ctx, tc, hub)
	}
	return c.dispatchRegistryTool(ctx, tc, hub)
}

// dispatchSubAgent runs the named sub-agent with an isolated context: only
// the sub-agent's own system prompt + one user message assembled from the
// invoke tool's {task, context} arguments. The sub-agent's events propagate
// up through the shared hub (already wired — same hub, the sub-agent's Run
// emits its own agent_start/token/agent_end). Its usage is folded back into
// the result so Confuse can aggregate.
func (c *Confuse) dispatchSubAgent(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
	sub, ok := c.subAgents[tc.Function.Name]
	if !ok {
		// Should not happen — NewConfuse validates presence — but defensive.
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
	content, usage, err := sub.Run(ctx, []Message{{Role: "user", Content: userMsg}}, hub)
	if err != nil {
		msg := err.Error()
		// sub.Run already streamed an agent_error event on its own (see
		// agent.Run implementations); fall back to a generic code here only
		// if it didn't. We surface the error text as the tool result so the
		// LLM can react.
		return toolResult{content: msg, isError: true, subUsage: &usage}
	}
	u := usage
	return toolResult{content: content, subUsage: &u}
}

func (c *Confuse) dispatchRegistryTool(ctx context.Context, tc ToolCall, hub *stream.Hub) toolResult {
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
