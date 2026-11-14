package handler

// Queue reconciler (session-message-queue, design D5): the backstop that
// drains queued entries a turn-goroutine pump never reached — the process
// died between claim release and pump, or the whole process restart left the
// queue populated. Modeled on internal/msgflush: one goroutine on the agent
// role, a ticker, WARN-only failures, multi-replica safe (the pump's atomic
// pop+claim transfer is the mutual exclusion), and it exits on Close.
//
// Each cycle also re-arms every queued run's run-key TTL (TouchQueued): the
// queue wait can outlast RunKeyTTL, and without the touch a long wait would
// expire the replay surface (events/meta) that the waiting SSE subscribers
// and later resumes read.

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/pkg/logger"
)

// QueueReconciler parameters. The interval is the crash-residue recovery
// latency bound; the per-cycle session cap bounds one cycle's work so a
// huge backlog cannot starve the ticker (the next cycle continues).
const (
	// queueReconcileInterval is the default scan period.
	queueReconcileInterval = 5 * time.Second
	// queueReconcileMaxSessions caps the sessions handled per cycle.
	queueReconcileMaxSessions = 64
)

// QueueReconciler scans the queue keyspace and continues claim-free session
// queues through the same pump path a turn's terminal bookkeeping uses.
type QueueReconciler struct {
	h        *MessageStreamHandler
	interval time.Duration
	// maxSessions caps the sessions handled per cycle (a var so tests can
	// shrink it; production takes queueReconcileMaxSessions).
	maxSessions int
	// cursor is the rotation offset into the scan result, advanced by the
	// number of sessions each cycle handled (review fix: a stable scan order
	// with more entries than the cap would otherwise re-process the same
	// head every cycle and starve the tail — both its pump chances AND its
	// TouchQueued TTL keep-alive). Owned by the run-loop goroutine.
	cursor int
	stop   chan struct{}
	done   chan struct{}
}

// NewQueueReconciler builds a reconciler over the streaming handler (it reuses
// the handler's pump, shutdown gate, and startTurn pipeline). interval <= 0
// takes the default.
func NewQueueReconciler(h *MessageStreamHandler, interval time.Duration) *QueueReconciler {
	if interval <= 0 {
		interval = queueReconcileInterval
	}
	return &QueueReconciler{
		h:           h,
		interval:    interval,
		maxSessions: queueReconcileMaxSessions,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start launches the reconcile loop; call exactly once.
func (r *QueueReconciler) Start() { go r.run() }

// Close stops the loop and waits (bounded) for the current cycle to end.
// Idempotent.
func (r *QueueReconciler) Close() {
	select {
	case <-r.stop:
		return
	default:
		close(r.stop)
	}
	select {
	case <-r.done:
	case <-time.After(2 * r.interval):
	}
}

func (r *QueueReconciler) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.cycle()
		}
	}
}

// cycle performs one bounded scan: touch every queued run's TTL (even behind
// a held claim — a long running turn must not let the waiting entries'
// replay surface expire), then try to continue each claim-free queue's head.
// The handling window ROTATES across cycles (see QueueReconciler.cursor), so
// a backlog larger than one cycle's cap still gets full coverage — the tail
// is neither pumped nor TTL-starved. Every failure is WARN-only: a
// reconciler problem must never disturb the running turns (the standing
// run-store contract).
func (r *QueueReconciler) cycle() {
	if r.h.newTurnsStopped() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sids, err := r.h.runs.Store.QueuedSessions(ctx)
	if err != nil {
		logger.FromContext(ctx).Warn("queue reconciler: session scan failed",
			zap.String("op", "handler.queue_reconciler"),
			zap.Error(err))
		return
	}
	n := len(sids)
	if n == 0 {
		r.cursor = 0
		return
	}
	handled := min(n, r.maxSessions)
	start := r.cursor % n
	for i := range handled {
		sid := sids[(start+i)%n]
		// TTL keep-alive for the waiting entries (best-effort).
		if entries, err := r.h.runs.Store.QueueEntries(ctx, sid); err == nil {
			for _, e := range entries {
				if err := r.h.runs.Store.TouchQueued(ctx, e.RunID); err != nil {
					logger.FromContext(ctx).Warn("queue reconciler: queued-run TTL touch failed",
						zap.String("op", "handler.queue_reconciler"),
						zap.String("run_id", e.RunID),
						zap.Error(err))
				}
			}
		}
		// Continue the head when the claim is free. The pump checks the
		// shutdown gate and the claim atomically; a held claim (a turn is
		// running or an admission just won the slot) leaves the queue alone.
		r.h.pumpQueuedNext(sid)
	}
	r.cursor = (start + handled) % n
}
