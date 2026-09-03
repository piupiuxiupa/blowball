// Package agent implements the multi-agent orchestration engine.
//
// The package exposes an Agent interface satisfied by the root Confucius agent
// and the generic dynamic SubAgent. The LLMClient interface decouples agent logic from any
// concrete LLM SDK so the agents are unit-testable with a fake client; the
// real openai-go-backed implementation lives in openai_client.go.
//
// Confucius is the root dispatcher; depth-eligible generic sub-agents may
// recursively spawn within the turn-wide budget. Every sub-agent sees only the
// task/snapshot supplied by its parent, never the user's full conversation.
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
	// Name returns the display label (Confucius or a dynamic instance label)
	// carried by StreamEvent.Agent.
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
	//     (the dispatcher) populates this; leaf agents (generic sub-agents)
	//     return nil and their parent folds their usage into its own breakdown.
	//
	// hub is the producer-facing EventHub view rather than a concrete *Hub so
	// the dispatcher can hand a sub-agent a run-tagging view
	// (stream.TaggedWithAgentRun) transparently: *Hub satisfies the interface and
	// agent bodies only ever call Send/SendCtx, so leaf agents stay unaware of
	// run identity (subagent-run-identity capability).
	Run(ctx context.Context, messages []Message, hub stream.EventHub) (assistantContent string, usage Usage, breakdown *TurnBreakdown, err error)
}

// SubAgentFactory builds a sub-agent for ONE spawn request (dynamic-
// subagents). The coordinator holds a factory — not a shared instance — so
// every dispatch constructs its own agent and the per-run mutable state (the
// side-effect flag backing ToolCallTracker, the round-cap flag backing
// RoundCapTracker) is isolated per invocation.
// Read-only construction inputs (config, LLM client, per-turn MCP manager)
// may be shared by capture. The factory must be safe to call concurrently.
type SubAgentFactory func(spec SubAgentSpec) (Agent, error)

// ToolCallTracker reports whether a Run dispatched a successful tool call. The
// retry wrapper uses it for idempotency: a possibly side-effecting agent is
// retried only when its most recent Run executed NO tool call.
type ToolCallTracker interface {
	// LastRunExecutedTool reports whether the most recent Run dispatched at
	// least one tool call that returned without error. Callers must invoke Run
	// before reading this; the value reflects the last completed Run.
	LastRunExecutedTool() bool
}

// RoundCapTracker is an optional capability implemented by every agent whose
// Run may hit its configured max_rounds cap. The dispatcher (Confucius)
// consults it after a sub-agent Run to propagate "a sub-agent hit its cap"
// into the turn-level usage.meta.round_capped flag (capability: agent
// round-cap). Each agent sets its flag when its loop exits via the cap path;
// callers must invoke Run before reading it. Confucius also implements it for
// uniformity, though it is never consulted as a sub-agent.
type RoundCapTracker interface {
	// LastRunHitCap reports whether the most recent Run exited its
	// tool-calling loop because max_rounds was reached (rather than a natural
	// stop). Callers must invoke Run before reading this; the value reflects
	// the last completed Run.
	LastRunHitCap() bool
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
// tool_calls (parallel) and which dynamic sub-agents fired, in dispatch
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

	// SubAgentInvocations lists every spawn dispatched this turn, in dispatch
	// order. Resume is a new invocation that reuses the same instance id.
	SubAgentInvocations []SubAgentInvocation

	// RoundCapped reports whether any agent (Confucius or a dispatched
	// sub-agent) hit its max_rounds cap this turn. Rendered into the done
	// event's usage.meta.round_capped (omitted when false so non-capped turns
	// serialize identically to before).
	RoundCapped bool

	// LastRoundContextTokens is the FINAL LLM round's prompt+completion — the
	// authoritative end-of-turn context size (context-compaction capability).
	// Rendered into usage.meta.context_tokens (omitted when 0) so the
	// streaming handler can persist it into turn_usage.context_tokens for the
	// next turn's preventive compaction check. Deliberately the LAST round,
	// never the cumulative sum of all rounds.
	LastRoundContextTokens int
}

// SubAgentInvocation records one dynamic dispatch for usage metadata. Order is
// the sequence in which the turn's coordinator accepted the spawn.
type SubAgentInvocation struct {
	AgentInstanceID  string `json:"agent_instance_id"`
	ParentInstanceID string `json:"parent_instance_id,omitempty"`
	Order            int    `json:"order"`
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
	Model    string
	Messages []Message
	Tools    []byte
	// MaxCompletionTokens is the output-token quota for the call (renamed
	// from MaxTokens by per-model-completion-budget): the resolved catalog
	// entry's max_completion_tokens for agent calls, a fixed budget for the
	// compaction summary, and 0 (uncapped) for title generation. The wire
	// family translates it — thinking:true sends max_completion_tokens,
	// thinking:false sends the legacy max_tokens.
	MaxCompletionTokens int
	// Thinking is the turn's WIRE-FAMILY marker (model-effort-v2): a copy of
	// the resolved catalog entry's thinking capability, not a mode switch.
	// true → the client always sends ReasoningEffort (literal "none"
	// included) and maps MaxCompletionTokens to max_completion_tokens;
	// false → no reasoning_effort, plain max_tokens. Sampling parameters
	// (temperature etc.) are never sent on either family.
	Thinking        bool
	ReasoningEffort string
	ResponseFormat  json.RawMessage
}

// ModelOverride is the per-turn model/effort configuration resolved from the
// chat request against the mandatory openai.models catalog (per-request-model-
// selection, model-effort-v2). The name is historical — since agents no
// longer carry model fields, this is not an "override" of agent config but
// the turn's ONLY source of model and effort; the structure survives so the
// AgentFactory.Build signature and existing tests stay put.
//
// It is ALWAYS non-zero in production: the handler resolves every request
// (a parameter-less request takes the default entry + the deployment default
// effort), and the factory injects the same value into every agent of the
// turn. Thinking is the wire-family marker (the entry's thinking capability
// copy); ReasoningEffort is the effective effort, never empty in the thinking
// family — "none" rides as a literal value so compatible gateways actually
// disable thinking. MaxCompletionTokens and LengthContinue carry the entry's
// output quota and per-entry finish_reason=length continuation policy
// (per-model-completion-budget) — one turn, one model, one quota across
// Confucius/generic sub-agents.
type ModelOverride struct {
	Model               string
	Thinking            bool
	ReasoningEffort     string
	MaxCompletionTokens int
	LengthContinue      config.LengthContinueConfig
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
func shouldDispatchToolCalls(ctx context.Context, resp LLMResponse) bool {
	if len(resp.ToolCalls) == 0 {
		return false
	}
	switch resp.FinishReason {
	case "tool_calls", "stop":
		return true
	}
	logger.FromContext(ctx).Warn("model returned tool_calls with unexpected finish_reason; treating as terminal",
		zap.String("finish_reason", resp.FinishReason),
		zap.Int("tool_calls", len(resp.ToolCalls)),
	)
	return false
}

// SpawnSubagentTool is the single dynamic sub-agent dispatch tool. Confucius
// and depth-eligible sub-agents intercept it BEFORE consulting tool.Registry;
// it never reaches the registry.
const SpawnSubagentTool = "spawn_subagent"

// UpdatePlanTool is the root-only semantic plan control tool. It is synthesized
// into Confucius's tools[] and intercepted before tool.Registry; dynamic
// sub-agents never receive it.
const UpdatePlanTool = "update_plan"

// Legacy fixed-role tool names remain only for readers of pre-migration event
// data; they are not model-facing and IsInvokeTool no longer recognizes them.
const (
	ToolInvokeChongzhi = "invoke_chongzhi"
	ToolInvokeLiang    = "invoke_liang"
)

// SpawnSubagentSchema returns the JSON Schema describing spawn_subagent.
func SpawnSubagentSchema() []byte { return append([]byte(nil), spawnArgsSchema...) }

// SpawnSubagentDescription is attached to the synthetic spawn_subagent tool.
const SpawnSubagentDescription = "Spawn an isolated generic sub-agent with a fresh context. The task prompt must be self-contained; tools may only narrow the caller's available tools. Use resume_agent_id to continue a capped or failed instance."

// IsInvokeTool reports whether name is the sub-agent dispatch tool recognized
// by Confucius and depth-eligible generic sub-agents.
func IsInvokeTool(name string) bool { return name == SpawnSubagentTool }

// UpdatePlanSchema returns the JSON Schema describing update_plan.
func UpdatePlanSchema() []byte { return append([]byte(nil), updatePlanArgsSchema...) }

// UpdatePlanDescription is attached to the synthetic update_plan tool.
const UpdatePlanDescription = "Replace the root agent's semantic plan with a complete snapshot. Multiple independent steps may be in_progress when they are being dispatched in parallel. Do not include a revision; the host assigns it."

const updatePlanArgsSchemaJSON = `{
  "type": "object",
  "properties": {
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 20,
      "items": {
        "type": "object",
        "properties": {
          "step": {
            "type": "string",
            "minLength": 1,
            "maxLength": 1000,
            "description": "One semantic work item."
          },
          "status": {
            "type": "string",
            "enum": ["pending", "in_progress", "completed"]
          }
        },
        "required": ["step", "status"],
        "additionalProperties": false
      }
    },
    "explanation": {
      "type": "string",
      "maxLength": 2000,
      "description": "Optional why the plan changed."
    }
  },
  "required": ["steps"],
  "additionalProperties": false
}`

var updatePlanArgsSchema = []byte(updatePlanArgsSchemaJSON)

const spawnArgsSchemaJSON = `{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "description": "The specific task for the sub-agent to perform."
    },
    "context": {
      "type": "string",
      "description": "Additional context the sub-agent needs to complete the task."
    },
    "name": {
      "type": "string",
      "description": "Short attribution label for this instance."
    },
    "tools": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional subset of the caller's available tools. Expansion is rejected."
    },
    "preset": {
      "type": "string",
      "description": "Optional named configuration template."
    },
    "resume_agent_id": {
      "type": "string",
      "description": "Stable agent_instance_id of a prior capped/error result to continue."
    }
  },
  "required": ["task"],
  "additionalProperties": false
}`

var spawnArgsSchema = []byte(spawnArgsSchemaJSON)

// SpawnToolArgs decodes the arguments emitted for spawn_subagent. Every field
// except task is optional.
type SpawnToolArgs struct {
	Task          string   `json:"task"`
	Context       string   `json:"context"`
	Name          string   `json:"name"`
	Tools         []string `json:"tools"`
	Preset        string   `json:"preset"`
	ResumeAgentID string   `json:"resume_agent_id"`
}
