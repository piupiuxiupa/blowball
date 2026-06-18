package handler

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// MergeEvents collapses adjacent token events from the same agent into a single
// event whose Content is the concatenation of the merged fragments. All other
// event boundaries (agent_start, agent_end, agent_error, tool_call, or a change
// in Agent) start a new output event so that total ordering and semantic
// boundaries are preserved.
func MergeEvents(events []stream.StreamEvent) []stream.StreamEvent {
	if len(events) == 0 {
		return nil
	}

	merged := make([]stream.StreamEvent, 0, len(events))
	var current *stream.StreamEvent
	for i := range events {
		e := events[i]
		if current != nil && e.Type == stream.EventToken && current.Type == stream.EventToken && current.Agent == e.Agent {
			current.Content += e.Content
			continue
		}
		if current != nil {
			merged = append(merged, *current)
		}
		current = &e
	}
	if current != nil {
		merged = append(merged, *current)
	}
	return merged
}

// UserMessage builds a model.Message for a user input without using StreamEvent.
// The user message occupies msg_index=0 within its turn and carries the request
// arrival timestamp so it sorts before the assistant events emitted later.
func UserMessage(sessionID, traceID, content string, msgTime time.Time) model.Message {
	return model.Message{
		SessionID: sessionID,
		MsgTime:   msgTime,
		Agent:     model.AgentUser,
		MsgIndex:  0,
		Role:      model.RoleUser,
		EventType: model.EventTypeMessage,
		Content:   content,
		TraceID:   traceID,
	}
}

// MessageFromEvent maps a StreamEvent produced by the orchestrator into a
// model.Message ready for persistence. Marker events (agent_start/agent_end)
// leave Role empty; token/tool_call events carry the OpenAI assistant role;
// tool_call content is JSON-encoded as {"tool_call_id":"...","name":..., "args":...};
// tool_result content is JSON-encoded as {"tool_call_id":"...","output":...}.
func MessageFromEvent(e stream.StreamEvent, sessionID, traceID string, msgIndex int, msgTime time.Time) (model.Message, error) {
	msg := model.Message{
		SessionID: sessionID,
		MsgTime:   msgTime,
		Agent:     e.Agent,
		MsgIndex:  msgIndex,
		TraceID:   traceID,
	}

	switch e.Type {
	case stream.EventToken:
		msg.EventType = model.EventTypeToken
		msg.Role = model.RoleAssistant
		msg.Content = e.Content
	case stream.EventToolCall:
		msg.EventType = model.EventTypeToolCall
		msg.Role = model.RoleAssistant
		toolCallID, _ := e.Meta[stream.MetaToolCallID].(string)
		args := e.Meta[stream.MetaArgs]
		if args == nil {
			args = map[string]any{}
		}
		payload := map[string]any{"tool_call_id": toolCallID, "name": e.Content, "args": args}
		b, err := json.Marshal(payload)
		if err != nil {
			return model.Message{}, fmt.Errorf("marshal tool_call content: %w", err)
		}
		msg.Content = string(b)
	case stream.EventToolResult:
		msg.EventType = model.EventTypeToolResult
		msg.Role = model.RoleTool
		toolCallID, _ := e.Meta[stream.MetaToolCallID].(string)
		output, err := marshalToolResultOutput(e.Content)
		if err != nil {
			return model.Message{}, fmt.Errorf("marshal tool_result content: %w", err)
		}
		payload := map[string]any{"tool_call_id": toolCallID, "output": output}
		b, err := json.Marshal(payload)
		if err != nil {
			return model.Message{}, fmt.Errorf("marshal tool_result content: %w", err)
		}
		msg.Content = string(b)
	case stream.EventAgentStart:
		msg.EventType = model.EventTypeAgentStart
		msg.Role = ""
	case stream.EventAgentEnd:
		msg.EventType = model.EventTypeAgentEnd
		msg.Role = ""
	case stream.EventAgentError:
		msg.EventType = model.EventTypeAgentError
		msg.Role = ""
		msg.Content = e.Content
	default:
		// Unknown event types are persisted verbatim with an empty role so the
		// store never silently drops events.
		msg.EventType = e.Type
		msg.Role = ""
		msg.Content = e.Content
	}

	return msg, nil
}

// marshalToolResultOutput returns the output value to serialize inside a
// tool_result payload. If content is valid JSON, it returns the decoded value
// so it serializes as structured data; otherwise it returns the raw string.
func marshalToolResultOutput(content string) (any, error) {
	if content == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(content), &v); err == nil {
		return v, nil
	}
	return content, nil
}
