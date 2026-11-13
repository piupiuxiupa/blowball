package handler

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/trace"
	"github.com/lush/blowball/pkg/run"
)

// pumpQueuedNext continues a session's queued head after a turn's terminal
// bookkeeping (session-message-queue, design D5): pop the head with the
// atomic pop+claim transfer, then run the tracked startup. Skipped entirely
// when the queue is disabled or the shutdown gate is already tripped (the
// entry stays enqueued for the post-restart reconciler).
func (h *MessageStreamHandler) pumpQueuedNext(sessionID string) {
	if h.queueMaxDepth <= 0 {
		return // queue disabled: byte-for-byte pre-capability behavior
	}
	if h.newTurnsStopped() {
		return // graceful shutdown: leave the entry to the post-restart reconciler
	}
	popCtx, popCancel := context.WithTimeout(context.Background(), 5*time.Second)
	entry, ok, err := h.runs.Store.PumpHead(popCtx, sessionID)
	popCancel()
	if err != nil {
		logger.FromContext(agent.WithSessionID(popCtx, sessionID)).Warn("queue pump: pop head failed; leaving the queue for the reconciler",
			zap.String("op", "handler.pump_queue"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		return
	}
	if !ok {
		return // empty queue, or the claim is held by a racing admission
	}
	h.pumpEntry(entry)
}

// pumpEntry drives one popped entry through the startup and hands it to
// startTurn. It is the shutdown barrier's tracked unit (review fix, round 3):
// the WaitGroup spans exactly the pop→spawn window the registry cannot see
// (Register happens after the send-time persistence), the preparation context
// is cancelled by the shutdown gate, and an aborted startup — cancelled
// preparation or the pre-durable-write checkpoint — returns the entry to the
// queue FRONT atomically and releases its claim, so neither a turn starts
// after graceful shutdown nor a popped message is lost at process exit.
func (h *MessageStreamHandler) pumpEntry(entry run.QueueEntry) {
	h.startupPumps.Add(1)
	defer h.startupPumps.Done()

	// The preparation context dies on the shutdown gate (the entry returns
	// to the queue) or the 60s bound.
	ctx, cancel := context.WithTimeout(h.shutdown.Context(), 60*time.Second)
	defer cancel()
	ctx = agent.WithUserID(agent.WithSessionID(trace.WithContext(ctx, entry.RunID), entry.SessionID), entry.UserID)

	req, fatalErr := h.turnRequestFromQueueEntry(ctx, entry)
	if fatalErr != nil {
		if ctx.Err() != nil {
			// Shutdown (or the bound) cancelled the preparation — the entry
			// never started; put it back rather than finalizing it as an
			// error.
			h.requeueAbortedPump(entry)
			return
		}
		// The entry genuinely cannot start (e.g. its session vanished
		// between enqueue and pump): finalize the run as error so the claim
		// the pump just transferred is released and waiting SSE subscribers
		// close.
		logger.FromContext(ctx).Warn("queue pump: entry cannot start; finalizing as error",
			zap.String("op", "handler.pump_queue"),
			zap.String("run_id", entry.RunID),
			zap.Error(fatalErr))
		finCtx, finCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finCancel()
		h.runs.Finalize(finCtx, entry.RunID, entry.SessionID, run.StatusError)
		return
	}
	started := h.startTurn(ctx, req)
	if started != nil && started.aborted != nil {
		// The shutdown checkpoint fired before any durable write: the entry
		// never started. Requeue it for the post-restart reconciler.
		h.requeueAbortedPump(entry)
		return
	}
}

// requeueAbortedPump returns a popped-but-unstarted entry to the queue front
// and releases its claim (atomic). A requeue failure loses the entry — the
// queue feature is Redis-dependent, so this is an ERROR, with the claim TTL
// as the session's backstop.
func (h *MessageStreamHandler) requeueAbortedPump(entry run.QueueEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.runs.Store.RequeueHead(ctx, entry.SessionID, entry); err != nil {
		logger.FromContext(ctx).Error("queue pump: aborted startup requeue failed; entry lost",
			zap.String("op", "handler.pump_queue"),
			zap.String("session_id", entry.SessionID),
			zap.String("run_id", entry.RunID),
			zap.Error(err))
		return
	}
	logger.FromContext(ctx).Info("queue pump: startup aborted; entry requeued for the post-restart reconciler",
		zap.String("op", "handler.pump_queue"),
		zap.String("session_id", entry.SessionID),
		zap.String("run_id", entry.RunID))
}

// turnRequestFromQueueEntry rebuilds a start-ready turn request from a
// queued entry snapshot (design D1: the snapshot — not the raw request
// parameters — is what travels, so the pumping process never re-resolves
// the request). userMsgTime is minted HERE (pump time): the ordering
// decision from design D4 — a queued row dated at arrival would sort before
// the previous turn's assistant rows in (msg_time, msg_index) history
// ordering.
func (h *MessageStreamHandler) turnRequestFromQueueEntry(ctx context.Context, entry run.QueueEntry) (*turnRequest, error) {
	// Re-validate the session survived until pump time. The enqueue path
	// verified ownership; DeleteSession clears the queue, so a miss here is
	// the tiny delete-vs-pump race — answer it by refusing to start.
	sess, err := h.sessSvc.GetSessionByID(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.UserID != entry.UserID {
		return nil, errors.New("session no longer exists or no longer belongs to the queueing user")
	}
	return &turnRequest{
		sessionID:        entry.SessionID,
		userID:           entry.UserID,
		traceID:          entry.RunID,
		content:          entry.Content,
		attachments:      entry.Attachments,
		sel:              selectionFromQueue(entry.Selection),
		rawToken:         entry.RawToken,
		userMsgTime:      time.Now(),
		contextCompacted: sess.ContextCompacted,
		metaCreatedAt:    entry.EnqueuedAt,
		fromQueue:        true,
	}, nil
}

// queueEntryFromRequest snapshots an admission-validated request into the
// queue's durable form (design D1): the resolved selection, the attachment
// references, the login JWT the pumped turn's tool executions will need,
// and the arrival time (display only — the persisted user row takes pump
// time, see turnRequestFromQueueEntry).
func queueEntryFromRequest(req *turnRequest, enqueuedAt time.Time) run.QueueEntry {
	return run.QueueEntry{
		RunID:       req.traceID,
		SessionID:   req.sessionID,
		UserID:      req.userID,
		Content:     req.content,
		Model:       req.sel.Model,
		Selection:   selectionToQueue(req.sel),
		Attachments: req.attachments,
		RawToken:    req.rawToken,
		EnqueuedAt:  enqueuedAt,
	}
}

// selectionToQueue / selectionFromQueue convert between the handler's
// resolved modelSelection and the queue's wire-stable snapshot form.
func selectionToQueue(sel modelSelection) run.QueueSelection {
	return run.QueueSelection{
		Model:               sel.Model,
		Thinking:            sel.Thinking,
		Vision:              sel.Vision,
		Effort:              sel.Effort,
		MaxContextTokens:    sel.MaxContextTokens,
		MaxCompletionTokens: sel.MaxCompletionTokens,
		LengthExpandStep:    sel.LengthContinue.ExpandStep,
		LengthMaxRetries:    sel.LengthContinue.MaxRetries,
	}
}

func selectionFromQueue(q run.QueueSelection) modelSelection {
	return modelSelection{
		Model:               q.Model,
		Thinking:            q.Thinking,
		Vision:              q.Vision,
		Effort:              q.Effort,
		MaxContextTokens:    q.MaxContextTokens,
		MaxCompletionTokens: q.MaxCompletionTokens,
		LengthContinue: config.LengthContinueConfig{
			ExpandStep: q.LengthExpandStep,
			MaxRetries: q.LengthMaxRetries,
		},
	}
}
