package stream

import (
	"context"
	"maps"
)

// MetaAgentInstanceID is the Meta key carrying the stable identity of a
// sub-agent instance. It survives resume and is the frontend's thread grouping
// key. MetaParentToolCallID carries the per-dispatch run identity: the id of
// the parent spawn tool_call. Both are injected exclusively by the dispatch
// layer — agent code never sets or reads them — so events are attributable
// end-to-end without the sub-agent knowing it is being tagged. Confucius's own
// events and user-message rows never carry either key.
const (
	MetaAgentInstanceID  = "agent_instance_id"
	MetaParentToolCallID = "parent_tool_call_id"
)

// EventHub is the producer-facing surface of a Hub: everything an agent needs
// to emit events. *Hub satisfies it directly; tagging views return a wrapping
// view that stamps dispatch identity before delegating. Agents code
// against this interface so the dispatch layer can hand a sub-agent a tagged
// view transparently — the sub-agent's signature cannot tell (and does not
// care) which it received.
type EventHub interface {
	Send(e StreamEvent) bool
	SendCtx(ctx context.Context, e StreamEvent) bool
}

// runTaggerHub is a producer-side view over an inner EventHub that stamps
// every outgoing event's Meta with agent instance/run identity. It embeds the inner
// EventHub so delegation is compile-time-guaranteed: only Send/SendCtx are
// overridden, and both delegate after tagging, so buffer-full drop semantics
// and shutdown races are byte-for-byte the inner hub's. The tagger never owns
// lifecycle — it is a producer-only wrapper and is discarded with the run.
type runTaggerHub struct {
	EventHub
	instanceID string
	runID      string
}

// TaggedWithRunID returns an EventHub view of inner that stamps
// Meta[MetaParentToolCallID]=runID onto every event sent through it. An empty
// runID returns inner unchanged (top-level runs carry no identity). Events
// that already carry the key pass through untouched, so wrapping is
// well-defined even over an already-tagged view (first stamp wins).
func TaggedWithRunID(inner EventHub, runID string) EventHub {
	return TaggedWithAgentRun(inner, "", runID)
}

// TaggedWithAgentRun returns an EventHub view of inner that stamps both the
// stable instance id and per-dispatch run id. Empty values are omitted, so
// top-level runs and the legacy run-only wrapper keep their prior wire shape.
func TaggedWithAgentRun(inner EventHub, instanceID, runID string) EventHub {
	if inner == nil || (instanceID == "" && runID == "") {
		return inner
	}
	return &runTaggerHub{EventHub: inner, instanceID: instanceID, runID: runID}
}

// Send implements EventHub: tag, then delegate to the inner hub.
func (t *runTaggerHub) Send(e StreamEvent) bool {
	return t.EventHub.Send(tagWithRunID(e, t.instanceID, t.runID))
}

// SendCtx implements EventHub: tag, then delegate to the inner hub.
func (t *runTaggerHub) SendCtx(ctx context.Context, e StreamEvent) bool {
	return t.EventHub.SendCtx(ctx, tagWithRunID(e, t.instanceID, t.runID))
}

// tagWithRunID returns e with Meta[MetaParentToolCallID] set to runID. A nil
// Meta is initialized; an existing value is never overwritten; a populated
// Meta is copied before mutation so a caller-shared map is never modified
// in place.
func tagWithRunID(e StreamEvent, instanceID, runID string) StreamEvent {
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	_, hasInstance := e.Meta[MetaAgentInstanceID]
	_, hasRun := e.Meta[MetaParentToolCallID]
	if hasInstance && hasRun {
		return e
	}
	m := make(map[string]any, len(e.Meta)+2)
	maps.Copy(m, e.Meta)
	if instanceID != "" && !hasInstance {
		m[MetaAgentInstanceID] = instanceID
	}
	if runID != "" && !hasRun {
		m[MetaParentToolCallID] = runID
	}
	e.Meta = m
	return e
}
