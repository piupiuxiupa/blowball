package handler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/stream"
)

// MergeEvents collapses adjacent token or reasoning events from the same agent
// into a single event whose Content is the concatenation of the merged fragments.
// All other event boundaries (agent_start, agent_end, agent_error, tool_call, or
// a change in Agent) start a new output event so that total ordering and semantic
// boundaries are preserved. Reasoning events are merged independently from token
// events so the reasoning/answer boundary is preserved.
//
// A change in run identity (Meta.parent_tool_call_id) is also a boundary:
// interleaved tokens of concurrent same-name sub-agent invocations
// (subagent-run-identity capability) must never merge across runs — a merged
// row can carry only one run id, and gluing two runs' sentences under the
// first run's id is exactly the garbling the capability fixes. Events without
// a run id (top-level turns) all compare equal, preserving the pre-change
// merge behavior byte-for-byte.
func MergeEvents(events []stream.StreamEvent) []stream.StreamEvent {
	if len(events) == 0 {
		return nil
	}

	merged := make([]stream.StreamEvent, 0, len(events))
	var current *stream.StreamEvent
	for i := range events {
		e := events[i]
		if current != nil && e.Type == current.Type && current.Agent == e.Agent &&
			runIDFromEvent(*current) == runIDFromEvent(e) &&
			(e.Type == stream.EventToken || e.Type == stream.EventReasoning) {
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

// deterministicClientMsgID mints the idempotency key for one turn message:
// {trace_id}:{msg_index}. A turn's messages may be persisted TWICE (mid-turn
// flush + turn-end save); both passes derive the same id from the same
// (trace, merged-event ordinal), so the messages table's UNIQUE index plus
// INSERT IGNORE collapse the redelivery to one row. msg_index 0 is the user
// message, 1+ the merged assistant events. Note the messages.client_msg_id
// column is CHAR(64) (migration 013 widened it) because a UUID trace_id alone
// is already 36 chars.
func deterministicClientMsgID(traceID string, msgIndex int) string {
	return traceID + ":" + strconv.Itoa(msgIndex)
}

// UserMessage builds a model.Message for a user input without using StreamEvent.
// The user message occupies msg_index=0 within its turn and carries the request
// arrival timestamp so it sorts before the assistant events emitted later.
// The client_msg_id is the deterministic {trace_id}:0 so a mid-turn flush and
// the turn-end save of the same user row collapse to one.
func UserMessage(sessionID, traceID, content string, msgTime time.Time) model.Message {
	return model.Message{
		SessionID:   sessionID,
		MsgTime:     msgTime,
		Agent:       model.AgentUser,
		MsgIndex:    0,
		Role:        model.RoleUser,
		EventType:   model.EventTypeMessage,
		Content:     content,
		TraceID:     traceID,
		ClientMsgID: deterministicClientMsgID(traceID, 0),
	}
}

// MessageFromEvent maps a StreamEvent produced by the orchestrator into a
// model.Message ready for persistence. Marker events (agent_start/agent_end)
// leave Role empty; token/tool_call events carry the OpenAI assistant role;
// tool_call content is JSON-encoded as {"tool_call_id":"...","name":..., "args":...};
// tool_result content is JSON-encoded as {"tool_call_id":"...","output":...}.
// The client_msg_id is the deterministic {trace_id}:{msg_index} keyed to the
// event's ordinal within the MERGED stream — callers must pass the merged
// index so the mid-turn flush and the turn-end save agree on every row.
func MessageFromEvent(e stream.StreamEvent, sessionID, traceID string, msgIndex int, msgTime time.Time) (model.Message, error) {
	msg := model.Message{
		SessionID:   sessionID,
		MsgTime:     msgTime,
		Agent:       e.Agent,
		MsgIndex:    msgIndex,
		TraceID:     traceID,
		ClientMsgID: deterministicClientMsgID(traceID, msgIndex),
		// Sub-agent invocation identity (subagent-run-identity capability):
		// stamped onto every event of a sub-agent Run by the dispatch layer.
		// Absent on Confucius's own events and on user rows (built by
		// UserMessage, which never sets it).
		RunID: runIDFromEvent(e),
	}

	switch e.Type {
	case stream.EventToken:
		msg.EventType = model.EventTypeToken
		msg.Role = model.RoleAssistant
		msg.Content = e.Content
	case stream.EventReasoning:
		msg.EventType = model.EventTypeReasoning
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

// runIDFromEvent extracts the sub-agent invocation identity from an event's
// Meta (stream.MetaParentToolCallID, injected by the dispatch layer's run
// tagger). It returns "" for top-level events — the empty value maps onto a
// NULL messages.run_id at the store layer.
func runIDFromEvent(e stream.StreamEvent) string {
	id, _ := e.Meta[stream.MetaParentToolCallID].(string)
	return id
}

// topLevelAssistantText concatenates the merged TOP-LEVEL assistant token
// runs of a turn (the memory-capture view of "what the assistant said",
// cross-session-memory capability): token events only, run-id-less only —
// every sub-agent Run event carries Meta.parent_tool_call_id
// (subagent-run-identity), so filtering on the empty run id excludes
// Chongzhi/Liang output by construction. Reasoning, tool_call/tool_result,
// and marker events are excluded; multi-round turns contribute every
// top-level token run (interim narration included), joined by a blank line —
// the memory extractor benefits from the whole exchange.
func topLevelAssistantText(merged []stream.StreamEvent) string {
	var runs []string
	for _, e := range merged {
		if e.Type != stream.EventToken || runIDFromEvent(e) != "" {
			continue
		}
		if e.Content == "" {
			continue
		}
		runs = append(runs, e.Content)
	}
	return strings.Join(runs, "\n\n")
}
