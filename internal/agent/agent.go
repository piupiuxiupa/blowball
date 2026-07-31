// Package agent implements the multi-agent orchestration engine.
//
// The package exposes an Agent interface that all agents (Confucius, Chongzhi,
// Liang) satisfy. The LLMClient interface decouples agent logic from any
// concrete LLM SDK so the agents are unit-testable with a fake client; the
// real openai-go-backed implementation lives in openai_client.go.
//
// Topology is flat: only Confucius may dispatch to other agents. Sub-agents
// (Chongzhi, Liang) see only the task description Confucius passes them, never
// the user's full conversation history.
package agent

import (
	"context"
	"encoding/json"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"go.uber.org/zap"
)

// Agent is the runtime contract every agent satisfies. Run executes one
// complete agent loop, streaming lifecycle and token events to hub and
// returning the final assistant content, the aggregated token usage, and the
// per-agent usage breakdown.
type Agent interface {
	// Name returns the agent's display name (Confucius | Chongzhi | Liang),
	// matching model.AgentConfucius/AgentChongzhi/AgentLiang and StreamEvent.Agent.
	Name() string

	// SystemPrompt returns the system prompt used to seed the agent's first
	// message. It is loaded from config.yaml at startup.
	SystemPrompt() string

	// RetryPolicy returns the agent's transient-error retry policy (capability
	// C). The dispatcher (Confucius) consults it to decide whether to retry a
	// sub-agent's failed LLM call. Leaf agents derive it from their config.
	RetryPolicy() config.AgentRetryConfig

	// Run executes the agent loop. messages is the conversation history in
	// OpenAI chat format (without the system prompt, which the implementation
	// prepends internally). Run streams agent_start/token/tool_call/agent_end
	// events to hub and returns:
	//   - assistantContent: the final assistant text,
	//   - usage: aggregated token usage across every LLM round in this Run,
	//   - breakdown: per-agent usage + orchestration metadata. Only Confucius
	//     (the dispatcher) populates this; leaf agents (Chongzhi/Liang)
	//     return nil and their parent folds their usage into its own breakdown.
	Run(ctx context.Context, messages []Message, hub *stream.Hub) (assistantContent string, usage Usage, breakdown *TurnBreakdown, err error)
}

// ToolCallTracker is an optional capability implemented by sub-agents whose
// Run may execute side-effecting tool calls (e.g. Chongzhi's xizhi write
// tools). The retry wrapper consults it for per-agent idempotency: a
// side-effecting agent is retried only when its most recent Run executed NO
// tool call. Read-only sub-agents (Liang) intentionally do NOT implement this
// interface so they remain unconditionally retryable.
type ToolCallTracker interface {
	// LastRunExecutedTool reports whether the most recent Run dispatched at
	// least one tool call that returned without error. Callers must invoke Run
	// before reading this; the value reflects the last completed Run.
	LastRunExecutedTool() bool
}

// TurnBreakdown is the per-agent usage attribution and orchestration metadata
// for one Confucius turn. It is the source of the done event's
// Meta.usage.by_agent and Meta.usage.meta, and is persisted verbatim into
// turn_usage.usage_json. Only Confucius assembles one (it is the only agent
// that dispatches sub-agents); leaf agents return nil and let the parent
// aggregate.
//
// Decision (design Open Question "Agent.Run signature"): the original lean was
// a bare `byAgent map[string]Usage` return value, but the done event also
// needs turn-level meta — whether any assistant round dispatched >=2
// tool_calls (parallel) and which invoke_* sub-agents fired, in dispatch
// order (sub_agent_invocations). Neither is reconstructable from the usage
// map (a map is unordered, and parallelism is per-round, not per-agent), so
// they must be carried alongside byAgent. Bundling both into a struct keeps
// the blast radius identical to a single new return value (one value, leaf
// agents return nil) while cleanly carrying meta, and avoids a 5-tuple Run
// signature. Recorded here per task 2.1.
type TurnBreakdown struct {
	// ByAgent maps agent display name -> that agent's aggregated usage for
	// the turn. Always contains Confucius's own usage under "Confucius"; adds
	// one entry per dispatched sub-agent under its display name. A sub-agent
	// that was invoked but failed still gets an entry (possibly zero usage) so
	// its dispatch is attributable.
	ByAgent map[string]Usage

	// Parallel reports whether any single assistant round dispatched >=2
	// tool_calls (the per-round definition of parallel dispatch).
	Parallel bool

	// SubAgentInvocations lists the invoke_* tool names dispatched this turn,
	// in first-seen (dispatch) order and de-duplicated.
	SubAgentInvocations []string
}

// Usage accumulates token counts for a single Run. Totals across rounds are
// summed by the agent loop before returning.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
}

// Add merges other into u in place. Used by the agent loop to aggregate
// usage across multiple LLM rounds and across sub-agent runs.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.ReasoningTokens += other.ReasoningTokens
}

// usageObject renders a Usage value into the per-agent object shape carried by
// the done event's Meta.usage.by_agent and .total. reasoning_tokens is only
// included when present (it is meaningful only for thinking/reasoning runs).
// This is the single source of truth for the usage-object shape shared by the
// done event (agent-orchestration spec) and turn_usage.usage_json
// (turn-cost-tracking spec).
func usageObject(u Usage) map[string]any {
	o := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.ReasoningTokens > 0 {
		o["reasoning_tokens"] = u.ReasoningTokens
	}
	return o
}

// Message is the agent-package's own chat message type. It mirrors the OpenAI
// chat schema (role/content/tool_calls/tool_call_id/name) without importing
// openai-go, keeping the public Agent interface SDK-agnostic. Callers that
// hold openai-go types convert at the boundary (see openai_client.go).
type Message struct {
	Role             string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"` // set on role="tool"
	Name             string     `json:"name,omitempty"`         // optional, set on role="tool"
}

// ToolCall represents one function-calling invocation emitted by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the function name and the raw JSON arguments string
// exactly as the model emitted them.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LLMClient is the per-agent LLM backend abstraction. Implementations stream
// chat completion tokens via onToken and reasoning content via onReasoning, and
// return the aggregated response. The interface deliberately avoids any
// openai-go types so the agent package can be tested with a fake client and so
// the concrete SDK-backed implementation can evolve without churning the agents.
type LLMClient interface {
	// StreamChat sends a streaming chat completion request and calls onToken
	// for every content delta and onReasoning for every reasoning delta the
	// model emits. It returns the final finish_reason ("stop" | "tool_calls" |
	// "length"), any assistant content, any tool_calls, and aggregated usage.
	// onToken and onReasoning must not be invoked after StreamChat returns;
	// implementations must abort streaming when ctx is cancelled.
	StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error, onReasoning func(string) error) (resp LLMResponse, err error)
}

// LLMRequest is the per-call payload handed to LLMClient.StreamChat. Tools is
// the OpenAI tools[] list already JSON-marshaled by tool.Registry.OpenAITools
// (or nil/empty when the agent has no tools). ResponseFormat, when non-empty,
// is a raw JSON response_format payload (e.g. {"type":"json_schema",...}) the
// client attaches to the chat completion to enable OpenAI structured output;
// sub-agents set this on their final tool-calling round when configured with
// an output_schema (capability A).
type LLMRequest struct {
	Model           string
	Messages        []Message
	Tools           []byte
	MaxTokens       int
	Temperature     float32
	Thinking        bool
	ReasoningEffort string
	ResponseFormat  json.RawMessage
}

// LLMResponse is the aggregated result of one streaming chat completion call.
type LLMResponse struct {
	FinishReason     string // "stop" | "tool_calls" | "length"
	Content          string
	ReasoningContent string // thinking/reasoning content from OpenAI reasoning models
	ToolCalls        []ToolCall
	Usage            Usage
}

// shouldDispatchToolCalls reports whether a model response that carries tool_calls
// should trigger a tool round. OpenAI's native API uses finish_reason="tool_calls"
// when emitting tool_calls, but some OpenAI-compatible endpoints report
// finish_reason="stop" (or an empty reason) even though tool_calls are present.
// We dispatch whenever tool_calls are present and the finish_reason is either
// "tool_calls" or "stop"; other reasons (length, content_filter, etc.) indicate
// truncation or filtering and are treated as terminal.
func shouldDispatchToolCalls(resp LLMResponse) bool {
	if len(resp.ToolCalls) == 0 {
		return false
	}
	switch resp.FinishReason {
	case "tool_calls", "stop":
		return true
	}
	logger.L().Warn("model returned tool_calls with unexpected finish_reason; treating as terminal",
		zap.String("finish_reason", resp.FinishReason),
		zap.Int("tool_calls", len(resp.ToolCalls)),
	)
	return false
}

// Sub-agent invocation tool names. Confucius intercepts these in its dispatch
// loop BEFORE consulting tool.Registry; they never reach the registry. The
// JSON schema for each is exported via InvokeToolSchema for the MCP handler
// (Phase 9) and unit tests.
const (
	ToolInvokeChongzhi = "invoke_chongzhi"
	ToolInvokeLiang    = "invoke_liang"
)

// InvokeToolSchema returns the JSON Schema describing the parameters Confucius
// must emit when invoking the named sub-agent via function calling. The
// schema is identical for both sub-agents: a required `task` and an optional
// `context`. Returns nil if name is not a recognized sub-agent invocation.
func InvokeToolSchema(name string) []byte {
	switch name {
	case ToolInvokeChongzhi, ToolInvokeLiang:
		return invokeArgsSchema
	}
	return nil
}

// InvokeToolDescription returns the human-readable description Confucius uses
// for the named sub-agent invocation tool. It is the single source of truth
// shared by the model-facing tools[] array (internal/agent/tools.go) and the
// MCP catalogue (internal/handler/mcp.go) so the two can never drift apart.
// Returns "" if name is not a recognized sub-agent invocation.
func InvokeToolDescription(name string) string {
	switch name {
	case ToolInvokeChongzhi:
		return InvokeChongzhiDescription
	case ToolInvokeLiang:
		return InvokeLiangDescription
	}
	return ""
}

// InvokeChongzhiDescription / InvokeLiangDescription are the descriptions
// Confucius attaches to the synthetic invoke_chongzhi / invoke_liang tools.
const (
	InvokeChongzhiDescription = "Invoke the Chongzhi (coding) sub-agent for code editing, file writing, or any task " +
		"that requires modifying files in the user's workspace. **Use it when a task MUST modify workspace files.** " +
		"**DO NOT use it for analysis-only tasks — use `invoke_liang`.**"
	InvokeLiangDescription = "Invoke the Liang (analysis) sub-agent for analysis, explanation, or reasoning; " +
		"**it MUST NOT modify files.** **DO NOT use it for file edits — use `invoke_chongzhi`.**"
)

// IsInvokeTool reports whether name is a sub-agent invocation tool recognized
// by the Confucius dispatch loop.
func IsInvokeTool(name string) bool {
	return name == ToolInvokeChongzhi || name == ToolInvokeLiang
}

const invokeArgsSchemaJSON = `{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "description": "The specific task for the sub-agent to perform."
    },
    "context": {
      "type": "string",
      "description": "Additional context the sub-agent needs to complete the task."
    }
  },
  "required": ["task"],
  "additionalProperties": false
}`

var invokeArgsSchema = []byte(invokeArgsSchemaJSON)

// InvokeToolArgs decodes the arguments string a model emits when calling a
// sub-agent. `context` is optional.
type InvokeToolArgs struct {
	Task    string `json:"task"`
	Context string `json:"context"`
}
