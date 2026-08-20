package handler

import (
	"context"
	"sync"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/stream"
)

// OrchestratorRunner is the agent-execution contract the MessageStreamHandler
// depends on. It runs one chat turn, streaming events to hub, and returns the
// raw event slice so the handler can persist the full assistant event stream,
// plus the turn's usage object (extracted from the terminal done event) so the
// handler can persist per-agent cost into turn_usage.
//
// Defining this locally lets the handler tests substitute a stub that writes
// canned events instead of driving the real agent loop. The production
// *agent.Orchestrator does not directly satisfy this interface (its Handle
// returns only error); wrap it with NewOrchestratorAdapter at wiring time.
type OrchestratorRunner interface {
	// Handle executes one full chat turn against workspaceRoot for userID,
	// streaming lifecycle and token events to hub. `messages` is the complete
	// conversation history including all prior turns and the current user
	// message. It returns when the turn is complete (terminal stop, error, or
	// context cancellation) and yields every event produced during the turn, in
	// order. The EventDone terminal event is forwarded to hub for the SSE wire
	// but is NOT included in the returned slice (it is not chat content);
	// instead its Meta.usage object is returned separately so the handler can
	// persist per-agent token cost without re-deriving it. The caller owns hub
	// and closes it after Handle returns.
	//
	// hooks carries the per-turn optional seams (context-compaction
	// capability): the between-rounds Round hook installed on the agent loop
	// and the event tap serving synchronized mid-turn snapshots. A zero
	// TurnHooks runs the turn hook-free (the pre-capability behavior).
	//
	// override (per-request-model-selection, model-effort-v2) is the
	// request-resolved turn config — (model, wire-family, effort) — injected
	// uniformly into all three agents. It is always non-zero: the handler
	// resolves every request against the mandatory catalog, and a
	// parameter-less request resolves to the default entry + deployment
	// default effort.
	Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub, hooks TurnHooks, override agent.ModelOverride) (events []stream.StreamEvent, usage map[string]any, err error)
}

// TurnHooks bundles the per-turn optional seams handed to OrchestratorRunner.
// Both fields are nil unless context compaction is configured.
type TurnHooks struct {
	// Round is the between-rounds mid-turn compaction hook (agent.RoundHook),
	// installed on the freshly-built Confucius for this turn.
	Round agent.RoundHook
	// Tap is served by the adapter's event-collection goroutine so Round can
	// take complete, synchronized snapshots of the collected event stream —
	// the source the mid-turn flush persists.
	Tap *TurnEventTap
}

// TurnEventTap synchronizes mid-turn flushes with the orchestrator adapter's
// event-collection goroutine (context-compaction capability). The collection
// goroutine owns the accumulated event slice and serves Snapshot requests
// inside its select loop; before replying it drains the hub's event channel
// to empty, so a snapshot taken while the agent loop is paused inside the
// round hook includes EVERY event emitted so far. That completeness matters:
// the flush persists rows under deterministic client_msg_ids keyed to the
// merged-event ordinal, and a snapshot that cut a token run mid-merge would
// store a truncated row that the turn-end save could never correct (the
// UNIQUE index keeps the first write).
type TurnEventTap struct {
	reqCh     chan chan []stream.StreamEvent
	done      chan struct{}
	closeOnce sync.Once
}

// NewTurnEventTap constructs a tap. Exactly one collection goroutine serves
// it for the lifetime of one turn; close is called when that goroutine exits.
func NewTurnEventTap() *TurnEventTap {
	return &TurnEventTap{
		reqCh: make(chan chan []stream.StreamEvent, 1),
		done:  make(chan struct{}),
	}
}

// Snapshot returns a complete copy of the events collected so far, or nil
// when the context is cancelled or the collection goroutine has exited (turn
// over). Callers treat nil as "abort the flush".
func (t *TurnEventTap) Snapshot(ctx context.Context) []stream.StreamEvent {
	reply := make(chan []stream.StreamEvent, 1)
	select {
	case t.reqCh <- reply:
	case <-ctx.Done():
		return nil
	case <-t.done:
		return nil
	}
	select {
	case events := <-reply:
		return events
	case <-ctx.Done():
		return nil
	case <-t.done:
		return nil
	}
}

// serve replies to one snapshot request with a copy of the collected events.
// Called by the collection goroutine after it drained the hub channel, so the
// copy is complete for the turn so far. The reply channel is buffered: an
// requester that gave up (ctx cancelled) leaves the value harmlessly.
func (t *TurnEventTap) serve(reply chan []stream.StreamEvent, events []stream.StreamEvent) {
	out := make([]stream.StreamEvent, len(events))
	copy(out, events)
	reply <- out
}

// close marks the tap dead; pending and future Snapshots return nil.
func (t *TurnEventTap) close() {
	t.closeOnce.Do(func() { close(t.done) })
}

// requests exposes the request channel for the collection goroutine's select
// loop; a nil-channel return lets a tap-free turn skip the case entirely.
func (t *TurnEventTap) requests() chan chan []stream.StreamEvent {
	if t == nil {
		return nil
	}
	return t.reqCh
}

// orchestratorAdapter wraps a *agent.Orchestrator to satisfy OrchestratorRunner.
// The underlying orchestrator's Handle returns only an error; we recover the
// full event stream by tapping the hub's events channel from a side goroutine
// while the orchestrator runs. The hub's events channel is a single-consumer
// channel, but Send/SendCtx push into it and the SSE writer is the consumer —
// so we cannot also read from it without stealing events.
//
// Instead, the adapter installs a *second* hub that the orchestrator writes
// to, fans every event out to the caller's hub (so the SSE writer still sees
// them) AND accumulates every event into a slice that becomes the returned
// event stream.
type orchestratorAdapter struct {
	inner *agent.Orchestrator
}

// NewOrchestratorAdapter wraps a *agent.Orchestrator as an OrchestratorRunner.
// The agent-role bootstrap should pass the result to NewMessageStreamHandler.
func NewOrchestratorAdapter(o *agent.Orchestrator) OrchestratorRunner {
	return &orchestratorAdapter{inner: o}
}

// Handle implements OrchestratorRunner.
func (a *orchestratorAdapter) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub, hooks TurnHooks, override agent.ModelOverride) ([]stream.StreamEvent, map[string]any, error) {
	// Tap side: drain innerHub.Events() in a goroutine, forwarding to the
	// caller's hub, accumulating the raw event stream, and capturing the done
	// event's usage object (without adding the done event to the persisted
	// stream).
	innerHub := stream.NewHub(stream.DefaultHubBufferSize)
	eventsCh := make(chan adapterResult, 1)
	tap := hooks.Tap
	tapReqs := tap.requests() // nil when no tap: the select case never fires

	go func() {
		if tap != nil {
			defer tap.close()
		}
		var events []stream.StreamEvent
		var usage map[string]any
		eventsDrain := innerHub.Events()
		done := innerHub.Done()
		process := func(e stream.StreamEvent) {
			// Mirror to the caller's hub. SendCtx blocks on a full buffer
			// until the SSE writer drains it; on ctx cancel or hub close
			// the event is dropped (the SSE writer is also observing ctx).
			hub.SendCtx(ctx, e)
			// Extract the usage object from the terminal done event so the
			// handler can persist per-agent cost. The done event itself is
			// still excluded from the returned event stream (it carries
			// usage metadata, not chat content).
			if e.Type == stream.EventDone {
				if u, ok := e.Meta[stream.MetaUsage].(map[string]any); ok {
					usage = u
				}
				return
			}
			events = append(events, e)
		}
		for {
			select {
			case e := <-eventsDrain:
				process(e)
			case req := <-tapReqs:
				// Mid-turn snapshot request (context-compaction capability):
				// priority-drain every event already enqueued on the hub so
				// the reply is complete for the turn so far. The agent loop
				// is paused inside the round hook while this runs, so no new
				// events can arrive mid-drain.
			snapshotDrain:
				for {
					select {
					case e := <-eventsDrain:
						process(e)
					default:
						break snapshotDrain
					}
				}
				tap.serve(req, events)
			case <-done:
				// Final drain: the orchestrator may have buffered agent_end /
				// done events into innerHub just before Close fired. Without
				// this drain, a Go select that lands on `done` while events
				// are still queued would silently drop them — observed in
				// Phase 11 integration tests as missing terminal events.
			drain:
				for {
					select {
					case e := <-eventsDrain:
						process(e)
					default:
						break drain
					}
				}
				eventsCh <- adapterResult{events: events, usage: usage}
				return
			case <-ctx.Done():
				eventsCh <- adapterResult{events: events, usage: usage}
				return
			}
		}
	}()

	err := a.inner.Handle(ctx, workspaceRoot, skillsDir, userID, messages, innerHub, hooks.Round, override)
	innerHub.Close()
	res := <-eventsCh
	return res.events, res.usage, err
}

// adapterResult bundles the drained event stream and the done event's usage
// object captured by the adapter's tap goroutine.
type adapterResult struct {
	events []stream.StreamEvent
	usage  map[string]any
}
