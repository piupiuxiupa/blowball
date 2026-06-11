package handler

import (
	"context"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/stream"
)

// OrchestratorRunner is the agent-execution contract the SessionHandler
// depends on. It runs one chat turn, streaming events to hub, and returns the
// final aggregated assistant content so the handler can persist it.
//
// Defining this locally lets the handler tests substitute a stub that writes
// canned events instead of driving the real agent loop. The production
// *agent.Orchestrator does not directly satisfy this interface (its Handle
// returns only error); wrap it with NewOrchestratorAdapter at wiring time.
type OrchestratorRunner interface {
	// Handle executes one full chat turn against workspaceRoot, streaming
	// lifecycle and token events to hub. It returns when the turn is complete
	// (terminal stop, error, or context cancellation) and yields the final
	// aggregated assistant content. The caller owns hub and closes it after
	// Handle returns.
	Handle(ctx context.Context, workspaceRoot, userMessage string, hub *stream.Hub) (assistantContent string, err error)
}

// orchestratorAdapter wraps a *agent.Orchestrator to satisfy OrchestratorRunner.
// The underlying orchestrator's Handle returns only an error; we recover the
// final assistant content by tapping the hub's events channel from a side
// goroutine while the orchestrator runs. The hub's events channel is a
// single-consumer channel, but Send/SendCtx push into it and the SSE writer
// is the consumer — so we cannot also read from it without stealing events.
//
// Instead, the adapter installs a *second* hub that the orchestrator writes
// to, fans every event out to the caller's hub (so the SSE writer still sees
// them) AND accumulates Confuse token deltas into a buffer that becomes the
// returned content. This double-buffering is the price of not changing the
// Phase 7 *agent.Orchestrator.Handle signature.
type orchestratorAdapter struct {
	inner *agent.Orchestrator
}

// NewOrchestratorAdapter wraps a *agent.Orchestrator as an OrchestratorRunner.
// Phase 10's main.go should pass the result to NewSessionHandler.
func NewOrchestratorAdapter(o *agent.Orchestrator) OrchestratorRunner {
	return &orchestratorAdapter{inner: o}
}

// Handle implements OrchestratorRunner.
func (a *orchestratorAdapter) Handle(ctx context.Context, workspaceRoot, userMessage string, hub *stream.Hub) (string, error) {
	// Tap side: drain innerHub.Events() in a goroutine, forwarding to the
	// caller's hub and accumulating Confuse token deltas.
	innerHub := stream.NewHub(stream.DefaultHubBufferSize)
	contentCh := make(chan string, 1)

	go func() {
		var content string
		events := innerHub.Events()
		done := innerHub.Done()
		for {
			select {
			case e := <-events:
				// Mirror to the caller's hub. SendCtx blocks on a full buffer
				// until the SSE writer drains it; on ctx cancel or hub close
				// the event is dropped (the SSE writer is also observing ctx).
				hub.SendCtx(ctx, e)
				if e.Type == stream.EventToken && e.Agent == stream.AgentConfuse {
					content += e.Content
				}
			case <-done:
				// Final drain: the orchestrator may have buffered agent_end /
				// done events into innerHub just before Close fired. Without
				// this drain, a Go select that lands on `done` while events
				// are still queued would silently drop them — observed in
				// Phase 11 integration tests as missing terminal events.
			drain:
				for {
					select {
					case e := <-events:
						hub.SendCtx(ctx, e)
						if e.Type == stream.EventToken && e.Agent == stream.AgentConfuse {
							content += e.Content
						}
					default:
						break drain
					}
				}
				contentCh <- content
				return
			case <-ctx.Done():
				contentCh <- content
				return
			}
		}
	}()

	err := a.inner.Handle(ctx, workspaceRoot, userMessage, innerHub)
	innerHub.Close()
	content := <-contentCh
	return content, err
}
