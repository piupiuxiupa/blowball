package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

// TurnRunHandler owns the turn-lifecycle endpoints of the turn-detach-resume
// capability: cancel-by-run-id and resume-events. Both are agent-partition
// routes; they validate ownership against the run meta in Redis and never
// touch the agent layer directly.
type TurnRunHandler struct {
	runs *run.Manager
}

// NewTurnRunHandler wires the turn-lifecycle endpoints. A nil Manager is a
// programming error (only the agent/all roles register these routes).
func NewTurnRunHandler(runs *run.Manager) *TurnRunHandler {
	return &TurnRunHandler{runs: runs}
}

// runStreamBlock is the XREAD BLOCK window of the SSE subscriber loop: it
// bounds how long an idle tail poll waits before re-checking the run's
// terminal status. Real Redis wakes the read the instant an entry lands, so
// this only sets the status-check cadence.
const runStreamBlock = time.Second

// streamIDPattern matches valid Redis Stream entry ids ("<ms>-<seq>") so an
// externally supplied Last-Event-ID can never inject a malformed XRANGE
// cursor.
var streamIDPattern = regexp.MustCompile(`^\d+(?:-\d+)?$`)

// sanitizeAfterID validates a resume cursor, mapping anything malformed
// (including the empty string) to "0" (replay from the beginning).
func sanitizeAfterID(id string) string {
	if streamIDPattern.MatchString(id) {
		return id
	}
	return "0"
}

// CancelTurn handles POST /api/v1/sessions/:session_id/turns/:run_id/cancel.
// Three states, in order:
//
//  1. the run is registered in THIS process → cancel its turn context; the
//     turn's own terminal path persists the partial output and finalizes;
//  2. the run heartbeats (another replica) → raise the Redis cancel flag; the
//     owning process's drainer observes it within one heartbeat period;
//  3. neither (dead run: crashed process) → force-clear: mark interrupted,
//     release the session claim, retire the keys.
//
// Cancelling an already-terminal run is an idempotent 200 reporting the
// terminal status. A run owned by another user/session reads as 404.
func (h *TurnRunHandler) CancelTurn(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	runID := c.Param("run_id")
	ctx := agent.WithSessionID(trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c)), sessionID)

	meta, ok, err := h.runs.Store.GetMeta(ctx, runID)
	if err != nil {
		logger.FromContext(ctx).Error("cancel: run meta read failed",
			zap.String("op", "handler.cancel_turn"),
			zap.String("run_id", runID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "run lookup failed"))
		return
	}
	if !ok || meta.SessionID != sessionID || meta.UserID != userID {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "run not found"))
		return
	}
	if run.Terminal(meta.Status) {
		c.JSON(http.StatusOK, turnStatusBody(runID, meta.Status))
		return
	}

	if h.runs.Registry != nil && h.runs.Registry.Cancel(runID) {
		// In-process cancel: the turn observes the cancellation, persists its
		// partial output, and finalizes the run as cancelled.
		c.JSON(http.StatusOK, turnStatusBody(runID, "cancelling"))
		return
	}
	alive, err := h.runs.Store.Alive(ctx, runID)
	if err != nil {
		logger.FromContext(ctx).Warn("cancel: alive check failed",
			zap.String("op", "handler.cancel_turn"),
			zap.String("run_id", runID), zap.Error(err))
	}
	if alive {
		// Running on another replica: flag it; that process's drainer
		// cancels the turn within one heartbeat period.
		if err := h.runs.Store.SetCancelFlag(ctx, runID); err != nil {
			logger.FromContext(ctx).Error("cancel: set cancel flag failed",
				zap.String("op", "handler.cancel_turn"),
				zap.String("run_id", runID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "cancel failed"))
			return
		}
		c.JSON(http.StatusOK, turnStatusBody(runID, "cancelling"))
		return
	}

	// Dead run: the owning process is gone. Force-clear so the session
	// unblocks immediately instead of waiting out the claim TTL.
	logger.FromContext(ctx).Warn("cancel: dead run force-cleared",
		zap.String("op", "handler.cancel_turn"),
		zap.String("run_id", runID))
	h.runs.Finalize(ctx, runID, sessionID, run.StatusInterrupted)
	c.JSON(http.StatusOK, turnStatusBody(runID, run.StatusInterrupted))
}

// TurnEvents handles GET /api/v1/sessions/:session_id/turns/:run_id/events —
// the resume endpoint. It replays the run's event log (from the beginning, or
// from Last-Event-ID) and tails live output until the run reaches a terminal
// state. Unknown/expired runs read as 410 so the client falls back to the
// ordinary history read.
func (h *TurnRunHandler) TurnEvents(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	runID := c.Param("run_id")
	ctx := agent.WithSessionID(trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c)), sessionID)

	meta, ok, err := h.runs.Store.GetMeta(ctx, runID)
	if err != nil {
		logger.FromContext(ctx).Error("turn events: run meta read failed",
			zap.String("op", "handler.turn_events"),
			zap.String("run_id", runID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "run lookup failed"))
		return
	}
	if !ok || meta.SessionID != sessionID || meta.UserID != userID {
		if !ok {
			c.JSON(http.StatusGone, errorBody("GONE", "run no longer replayable"))
			return
		}
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "run not found"))
		return
	}

	err = writeRunEventStream(ctx, c.Writer, h.runs, runID,
		sanitizeAfterID(c.GetHeader("Last-Event-ID")),
		map[string]string{"X-Run-Id": runID}, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.FromContext(ctx).Warn("turn events: sse write returned error",
			zap.String("op", "handler.turn_events"),
			zap.String("run_id", runID), zap.Error(err))
	}
}

func turnStatusBody(runID, status string) gin.H {
	return gin.H{"run_id": runID, "status": status}
}

// writeRunEventStream subscribes to a run's event log and writes SSE frames:
// replay after the cursor, then live tail. It is the SINGLE subscription
// implementation shared by the originating POST response and the resume
// endpoint — both are just readers of run:{rid}:events (turn-detach-resume
// design D2). turnDone (originating connection only; nil for attach) is the
// owning turn's completion signal — the last-resort close condition when the
// store is unreachable (e.g. Redis down: the turn finishes but neither the
// done event nor the terminal status can ever reach the log). The stream
// closes when:
//
//   - the done event has been written to the client and the log reads empty
//     (in-band fast path; nothing can follow done),
//   - the run reaches a terminal status and the log is drained to its end
//     (covers cancelled turns, whose done event never reaches the log),
//   - the run's keys expired mid-stream (retention window passed),
//   - a status-running run's heartbeat expired (dead run): a synthesized
//     terminal done(error=interrupted) is emitted, the run is marked
//     interrupted, and the session claim is released,
//   - the owning turn completed (turnDone; degraded no-Redis path), or
//   - the caller's context is cancelled (client disconnect — the turn
//     itself keeps running by design).
func writeRunEventStream(ctx context.Context, w http.ResponseWriter, runs *run.Manager, runID, after string, extraHeaders map[string]string, turnDone <-chan struct{}) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("run events: http.ResponseWriter does not implement http.Flusher")
	}
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	if err := stream.PrepareSSE(w); err != nil {
		return err
	}

	cursor := sanitizeAfterID(after)
	sawDone := false
	for {
		entries, err := runs.Store.ReadEvents(ctx, runID, cursor, runStreamBlock)
		if err != nil {
			// Transient store error: back off briefly and keep the stream
			// open rather than tearing down a live subscription — unless the
			// owning turn already finished (no signal can ever arrive on
			// this stream again), which ends the response.
			logger.FromContext(ctx).Warn("run events: read failed",
				zap.String("op", "run_events"),
				zap.String("run_id", runID), zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-turnDone:
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		for _, ent := range entries {
			if err := stream.WriteSSEFrame(w, ent.Event, ent.ID); err != nil {
				return err
			}
			cursor = ent.ID
			if ent.Event.Type == stream.EventDone {
				sawDone = true
			}
		}
		if len(entries) > 0 {
			flusher.Flush()
			continue
		}

		// In-band fast path: the done event is the orchestrator's terminal
		// marker and nothing can follow it in the log, so once it has been
		// written to the client and the log reads empty, the stream is over
		// — no need to wait for the terminal status write (which covers the
		// cancel path, where no done event reaches the log).
		if sawDone {
			return nil
		}

		// Idle poll: decide whether the stream is over.
		meta, ok, err := runs.Store.GetMeta(ctx, runID)
		if err != nil {
			// Transient meta error: retry next poll unless the turn itself
			// already completed (degraded path — nothing more can arrive).
			select {
			case <-turnDone:
				return nil
			default:
			}
			continue
		}
		if !ok {
			// Keys expired mid-stream (retention window). The client already
			// has everything replayable; ending the stream is the contract.
			return nil
		}
		if run.Terminal(meta.Status) {
			// Final safety drain: events cannot follow the terminal status
			// (it is written strictly after the drainer drains), but a
			// cheap tail read guarantees the client saw the done event.
			tail, _ := runs.Store.ReadEvents(ctx, runID, cursor, 0)
			for _, ent := range tail {
				if err := stream.WriteSSEFrame(w, ent.Event, ent.ID); err != nil {
					return err
				}
			}
			if len(tail) > 0 {
				flusher.Flush()
			}
			return nil
		}
		if meta.Status == run.StatusRunning {
			alive, err := runs.Store.Alive(ctx, runID)
			if err == nil && !alive {
				// Dead run: the owning process died mid-turn. Synthesize the
				// terminal done the run will never emit, mark it interrupted,
				// and release the session so the user can send again.
				logger.FromContext(ctx).Warn("run events: dead run detected",
					zap.String("op", "run_events"),
					zap.String("run_id", runID),
					zap.String("meta_session_id", meta.SessionID))
				ev := stream.DoneEvent(map[string]any{"error": "interrupted: run ended unexpectedly"})
				if err := stream.WriteSSEFrame(w, ev, ""); err != nil {
					return err
				}
				flusher.Flush()
				runs.Finalize(ctx, runID, meta.SessionID, run.StatusInterrupted)
				return nil
			}
		}
	}
}
