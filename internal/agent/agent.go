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

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"go.uber.org/zap"
)

// Agent is the runtime contract every agent satisfies. Run executes one
// complete agent loop, streaming lifecycle and token events to hub and
// returning the final assistant content and aggregated token usage.
type Agent interface {
	// Name returns the agent's display name (Confucius | Chongzhi | Liang),
	// matching model.AgentConfucius/AgentChongzhi/AgentLiang and StreamEvent.Agent.
	Name() string

	// SystemPrompt returns the system prompt used to seed the agent's first
	// message. It is loaded from config.yaml at startup.
	SystemPrompt() string

	// Run executes the agent loop. messages is the conversation history in
	// OpenAI chat format (without the system prompt, which the implementation
	// prepends internally). Run streams agent_start/token/tool_call/agent_end
	// events to hub and returns the final assistant content plus aggregated
	// usage across every LLM round within this Run.
	Run(ctx context.Context, messages []Message, hub *stream.Hub) (assistantContent string, usage Usage, err error)
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
// (or nil/empty when the agent has no tools).
type LLMRequest struct {
	Model           string
	Messages        []Message
	Tools           []byte
	MaxTokens       int
	Temperature     float32
	Thinking        bool
	ReasoningEffort string
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
