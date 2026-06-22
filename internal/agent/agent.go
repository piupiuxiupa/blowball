// Package agent implements the multi-agent orchestration engine.
//
// The package exposes an Agent interface that all agents (Confuse, Chongzhi,
// Liang) satisfy. The LLMClient interface decouples agent logic from any
// concrete LLM SDK so the agents are unit-testable with a fake client; the
// real openai-go-backed implementation lives in openai_client.go.
//
// Topology is flat: only Confuse may dispatch to other agents. Sub-agents
// (Chongzhi, Liang) see only the task description Confuse passes them, never
// the user's full conversation history.
package agent

import (
	"context"

	"github.com/lush/blowball/internal/stream"
)

// Agent is the runtime contract every agent satisfies. Run executes one
// complete agent loop, streaming lifecycle and token events to hub and
// returning the final assistant content and aggregated token usage.
type Agent interface {
	// Name returns the agent's display name (Confuse | Chongzhi | Liang),
	// matching model.AgentConfuse/AgentChongzhi/AgentLiang and StreamEvent.Agent.
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
}

// Add merges other into u in place. Used by the agent loop to aggregate
// usage across multiple LLM rounds and across sub-agent runs.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
}

// Message is the agent-package's own chat message type. It mirrors the OpenAI
// chat schema (role/content/tool_calls/tool_call_id/name) without importing
// openai-go, keeping the public Agent interface SDK-agnostic. Callers that
// hold openai-go types convert at the boundary (see openai_client.go).
type Message struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // set on role="tool"
	Name       string     `json:"name,omitempty"`         // optional, set on role="tool"
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
// chat completion tokens via onToken and return the aggregated response. The
// interface deliberately avoids any openai-go types so the agent package can
// be tested with a fake client and so the concrete SDK-backed implementation
// can evolve without churning the agents.
type LLMClient interface {
	// StreamChat sends a streaming chat completion request and calls onToken
	// for every content delta the model emits. It returns the final
	// finish_reason ("stop" | "tool_calls" | "length"), any assistant content,
	// any tool_calls, and aggregated usage. onToken must not be invoked after
	// StreamChat returns; implementations must abort streaming when ctx is
	// cancelled.
	StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error) (resp LLMResponse, err error)
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

// Sub-agent invocation tool names. Confuse intercepts these in its dispatch
// loop BEFORE consulting tool.Registry; they never reach the registry. The
// JSON schema for each is exported via InvokeToolSchema for the MCP handler
// (Phase 9) and unit tests.
const (
	ToolInvokeChongzhi = "invoke_chongzhi"
	ToolInvokeLiang    = "invoke_liang"
)

// InvokeToolSchema returns the JSON Schema describing the parameters Confuse
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

// IsInvokeTool reports whether name is a sub-agent invocation tool recognized
// by the Confuse dispatch loop.
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
