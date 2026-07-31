package handler

import (
	"context"

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
	Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub) (events []stream.StreamEvent, usage map[string]any, err error)
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
func (a *orchestratorAdapter) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub) ([]stream.StreamEvent, map[string]any, error) {
	// Tap side: drain innerHub.Events() in a goroutine, forwarding to the
	// caller's hub, accumulating the raw event stream, and capturing the done
	// event's usage object (without adding the done event to the persisted
	// stream).
	innerHub := stream.NewHub(stream.DefaultHubBufferSize)
	eventsCh := make(chan adapterResult, 1)

	go func() {
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

	err := a.inner.Handle(ctx, workspaceRoot, skillsDir, userID, messages, innerHub)
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
