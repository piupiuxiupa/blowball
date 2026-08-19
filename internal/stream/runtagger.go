package stream

import (
	"context"
	"maps"
)

// MetaParentToolCallID is the Meta key carrying a sub-agent invocation's run
// identity: the id of the parent (invoke_*) tool_call that spawned the run
// (subagent-run-identity capability). It is injected exclusively by the
// dispatch layer via TaggedWithRunID — agent code never sets or reads it — so
// a run's events are attributable end-to-end (SSE → messages.run_id → frontend
// segmentation) without the sub-agent knowing it is being tagged. Confucius's
// own events and user-message rows never carry the key.
const MetaParentToolCallID = "parent_tool_call_id"

// EventHub is the producer-facing surface of a Hub: everything an agent needs
// to emit events. *Hub satisfies it directly; TaggedWithRunID returns a
// wrapping view that stamps run identity before delegating. Agents code
// against this interface so the dispatch layer can hand a sub-agent a tagged
// view transparently — the sub-agent's signature cannot tell (and does not
// care) which it received.
type EventHub interface {
	Send(e StreamEvent) bool
	SendCtx(ctx context.Context, e StreamEvent) bool
}

// runTaggerHub is a producer-side view over an inner EventHub that stamps
// every outgoing event's Meta with MetaParentToolCallID. It embeds the inner
// EventHub so delegation is compile-time-guaranteed: only Send/SendCtx are
// overridden, and both delegate after tagging, so buffer-full drop semantics
// and shutdown races are byte-for-byte the inner hub's. The tagger never owns
// lifecycle — it is a producer-only wrapper and is discarded with the run.
type runTaggerHub struct {
	EventHub
	runID string
}

// TaggedWithRunID returns an EventHub view of inner that stamps
// Meta[MetaParentToolCallID]=runID onto every event sent through it. An empty
// runID returns inner unchanged (top-level runs carry no identity). Events
// that already carry the key pass through untouched, so wrapping is
// well-defined even over an already-tagged view (first stamp wins).
func TaggedWithRunID(inner EventHub, runID string) EventHub {
	if inner == nil || runID == "" {
		return inner
	}
	return &runTaggerHub{EventHub: inner, runID: runID}
}

// Send implements EventHub: tag, then delegate to the inner hub.
func (t *runTaggerHub) Send(e StreamEvent) bool {
	return t.EventHub.Send(tagWithRunID(e, t.runID))
}

// SendCtx implements EventHub: tag, then delegate to the inner hub.
func (t *runTaggerHub) SendCtx(ctx context.Context, e StreamEvent) bool {
	return t.EventHub.SendCtx(ctx, tagWithRunID(e, t.runID))
}

// tagWithRunID returns e with Meta[MetaParentToolCallID] set to runID. A nil
// Meta is initialized; an existing value is never overwritten; a populated
// Meta is copied before mutation so a caller-shared map is never modified
// in place.
func tagWithRunID(e StreamEvent, runID string) StreamEvent {
	if e.Meta == nil {
		e.Meta = map[string]any{MetaParentToolCallID: runID}
		return e
	}
	if _, ok := e.Meta[MetaParentToolCallID]; ok {
		return e
	}
	m := make(map[string]any, len(e.Meta)+1)
	maps.Copy(m, e.Meta)
	m[MetaParentToolCallID] = runID
	e.Meta = m
	return e
}
