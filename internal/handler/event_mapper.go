package handler

import (
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
			eventIdentityKey(*current) == eventIdentityKey(e) &&
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

// runIDFromEvent extracts the sub-agent invocation identity from an event's
// Meta (stream.MetaParentToolCallID, injected by the dispatch layer's run
// tagger). It returns "" for top-level events — the empty value maps onto a
// NULL messages.run_id at the store layer.
func runIDFromEvent(e stream.StreamEvent) string {
	id, _ := e.Meta[stream.MetaParentToolCallID].(string)
	return id
}

// agentInstanceIDFromEvent extracts the stable sub-agent instance identity.
// It is empty on top-level and legacy events, mapping to NULL at the store.
func agentInstanceIDFromEvent(e stream.StreamEvent) string {
	id, _ := e.Meta[stream.MetaAgentInstanceID].(string)
	return id
}

func eventIdentityKey(e stream.StreamEvent) string {
	return agentInstanceIDFromEvent(e) + "\x00" + runIDFromEvent(e)
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
		if e.Type != stream.EventToken || runIDFromEvent(e) != "" || agentInstanceIDFromEvent(e) != "" {
			continue
		}
		if e.Content == "" {
			continue
		}
		runs = append(runs, e.Content)
	}
	return strings.Join(runs, "\n\n")
}
