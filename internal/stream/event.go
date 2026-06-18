// Package stream provides StreamEvent primitives, a buffered event hub, and an
// SSE writer for streaming agent output to HTTP clients.
//
// Events flow from agents through a *Hub (a buffered channel) and are consumed
// by WriteSSE which serializes them to the SSE wire format. The hub decouples
// producer (agent goroutines) from consumer (the SSE handler).
package stream

import "encoding/json"

// Event type values, emitted as StreamEvent.Type and used as the SSE event name.
const (
	EventAgentStart = "agent_start"
	EventToken      = "token"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventAgentEnd   = "agent_end"
	EventAgentError = "agent_error"
	EventDone       = "done"
	// EventMessage is a sentinel used for user message rows persisted to the
	// messages table; it is never emitted as an SSE event.
	EventMessage = "message"
)

// Agent names. These mirror model.AgentConfuse/AgentChongzhi/AgentLiang but are
// declared locally to avoid an import cycle (model depends on packages that may
// eventually depend on stream). Keep these in sync with internal/model/message.go.
const (
	AgentConfuse  = "Confuse"
	AgentChongzhi = "Chongzhi"
	AgentLiang    = "Liang"
)

// Meta keys used by event constructors.
const (
	MetaArgs       = "args"
	MetaUsage      = "usage"
	MetaCode       = "error_code"
	MetaDetail     = "error_detail"
	MetaToolCallID = "tool_call_id"
)

// StreamEvent is the unit of data exchanged between agents and the SSE consumer.
// Agents emit events into a *Hub; WriteSSE serializes them as
// "event: <Type>\ndata: <json>\n\n".
type StreamEvent struct {
	Type    string         `json:"type"`
	Agent   string         `json:"agent"`
	Content string         `json:"content"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// TokenEvent builds a token streaming event for a single content chunk.
func TokenEvent(agent, content string) StreamEvent {
	return StreamEvent{Type: EventToken, Agent: agent, Content: content}
}

// AgentStartEvent marks the start of an agent's execution. Frontends use this to
// switch UI focus to the named agent.
func AgentStartEvent(agent string) StreamEvent {
	return StreamEvent{Type: EventAgentStart, Agent: agent}
}

// AgentEndEvent marks successful completion of an agent's execution.
func AgentEndEvent(agent string) StreamEvent {
	return StreamEvent{Type: EventAgentEnd, Agent: agent}
}

// AgentErrorEvent reports an agent failure. `code` is a short machine-readable
// error code (placed in Meta[MetaCode]); `message` is the human-readable detail
// carried in Content.
func AgentErrorEvent(agent, message, code string) StreamEvent {
	return StreamEvent{
		Type:    EventAgentError,
		Agent:   agent,
		Content: message,
		Meta:    map[string]any{MetaCode: code},
	}
}

// ToolCallEvent reports that an agent invoked a tool. `toolCallID` correlates
// the call with its result event(s); `args` is marshaled to Meta["args"]; if
// marshaling fails the raw value is stored under Meta["args_raw"] and the
// marshal error under Meta["args_error"].
func ToolCallEvent(agent, toolCallID, toolName string, args any) StreamEvent {
	e := StreamEvent{
		Type:    EventToolCall,
		Agent:   agent,
		Content: toolName,
		Meta:    map[string]any{MetaToolCallID: toolCallID},
	}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			e.Meta[MetaArgs+"_error"] = err.Error()
			e.Meta[MetaArgs+"_raw"] = args
			return e
		}
		// Store as json.RawMessage so it serializes inline rather than as a
		// base64 string, preserving nested object structure for the frontend.
		e.Meta[MetaArgs] = json.RawMessage(b)
	}
	return e
}

// ToolResultEvent reports the outcome of a tool invocation. `toolCallID`
// matches the ID emitted by the preceding ToolCallEvent; `output` is the
// tool's serialized result (or error text) carried in Content.
func ToolResultEvent(agent, toolCallID, output string) StreamEvent {
	return StreamEvent{
		Type:    EventToolResult,
		Agent:   agent,
		Content: output,
		Meta:    map[string]any{MetaToolCallID: toolCallID},
	}
}

// DoneEvent signals the end of the stream. `usage` (e.g. total token counts and
// per-agent breakdowns) is placed under Meta["usage"].
func DoneEvent(usage map[string]any) StreamEvent {
	e := StreamEvent{Type: EventDone, Meta: map[string]any{}}
	if usage != nil {
		e.Meta[MetaUsage] = usage
	}
	return e
}
