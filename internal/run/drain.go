package run

import (
	"context"
	"maps"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
)

// MetaRunID is the Meta key carrying the owning run's id on every event read
// from a run event log. The drainer stamps it before appending so SSE
// subscribers (originating connection and resume endpoint alike) can attribute
// every event — including sub-agent events, which additionally carry
// parent_tool_call_id — to the run.
const MetaRunID = "run_id"

// StartDrainer pumps the turn hub into the run store: it is the hub's single
// consumer, appending every event (done included) to run:{rid}:events,
// refreshing the heartbeat, and polling the cross-process cancel flag every
// HeartbeatEvery. It returns a channel closed after the final drain, so the
// caller can order terminal bookkeeping (SetStatus) strictly after the last
// event is durable in the log.
//
// The drainer outlives the HTTP request by design (turn-detach-resume): it
// runs on the turn's own lifetime, not any request context, and exits when
// the hub closes — i.e. when the orchestrator goroutine returns. Store errors
// are WARNed and never stop the turn: a Redis outage costs replayability,
// not the turn itself.
func StartDrainer(hub *stream.Hub, store Store, reg *Registry, runID string, heartbeatEvery time.Duration) <-chan struct{} {
	if heartbeatEvery <= 0 {
		heartbeatEvery = HeartbeatEvery
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		events := hub.Events()
		hdone := hub.Done()
		beat := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := store.Heartbeat(ctx, runID); err != nil {
				warn("drainer: heartbeat failed", runID, err)
			}
			if flagged, err := store.TakeCancelFlag(ctx, runID); err != nil {
				warn("drainer: cancel-flag poll failed", runID, err)
			} else if flagged {
				// Cross-replica cancel observed: cancel the local turn
				// context; the normal terminal path then persists the
				// partial output and finalizes the run as cancelled.
				logger.L().Info("run cancelled via redis flag", zap.String("run_id", runID))
				reg.Cancel(runID)
			}
		}
		beat() // first heartbeat immediately so alive exists from turn start
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		appendOne := func(e stream.StreamEvent) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := store.AppendEvent(ctx, runID, stampRunID(e, runID)); err != nil {
				warn("drainer: append event failed", runID, err)
			}
		}
		for {
			select {
			case e := <-events:
				appendOne(e)
			case <-ticker.C:
				beat()
			case <-hdone:
				// Final drain: buffered events may still sit on the channel
				// when Done fires (mirrors the adapter's own final drain).
			drain:
				for {
					select {
					case e := <-events:
						appendOne(e)
					default:
						break drain
					}
				}
				return
			}
		}
	}()
	return drained
}

// stampRunID returns e with Meta[MetaRunID]=runID, copying a populated Meta
// before mutation (same discipline as stream.tagWithRunID: first stamp wins,
// caller-shared maps are never modified in place).
func stampRunID(e stream.StreamEvent, runID string) stream.StreamEvent {
	if e.Meta == nil {
		e.Meta = map[string]any{MetaRunID: runID}
		return e
	}
	if _, ok := e.Meta[MetaRunID]; ok {
		return e
	}
	m := make(map[string]any, len(e.Meta)+1)
	maps.Copy(m, e.Meta)
	m[MetaRunID] = runID
	e.Meta = m
	return e
}

// warn logs a run-lifecycle warning. Every run-store failure is non-fatal by
// contract, so a shared one-liner keeps the WARN shape uniform.
func warn(msg, runID string, err error) {
	logger.L().Warn(msg,
		zap.String("op", "run"),
		zap.String("run_id", runID),
		zap.Error(err))
}
