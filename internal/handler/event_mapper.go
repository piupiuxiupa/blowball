package handler

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// MessageFromEvent maps a StreamEvent produced by the orchestrator into a
// model.Message ready for persistence. Marker events (agent_start/agent_end)
// leave Role empty; token/tool_call events carry the OpenAI assistant role;
// tool_call content is JSON-encoded as {"name":..., "args":...}.
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
		args := e.Meta[stream.MetaArgs]
		if args == nil {
			args = map[string]any{}
		}
		payload := map[string]any{"name": e.Content, "args": args}
		b, err := json.Marshal(payload)
		if err != nil {
			return model.Message{}, fmt.Errorf("marshal tool_call content: %w", err)
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
