package handler

// Queue read endpoint (session-message-queue): GET /api/v1/sessions/:session_id/queue
// serves the session's queued entries in enqueue order — the authoritative
// channel for a reloaded frontend to rebuild its pending-message bubbles
// (the queued messages are not in GET /messages until their turns start;
// see the pump-time persistence decision, design D4). It lives in the API
// partition: it only reads the run Store's queue primitives — no registry,
// no orchestrator — so the api role serves it exactly like the history read.

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
)

// QueueHandler serves the queue read endpoint.
type QueueHandler struct {
	sessSvc *service.SessionService
	runs    *run.Manager
}

// NewQueueHandler wires the queue read endpoint. The Manager carries only the
// Store in the api role (list-marker reads only — the Registry is nil).
func NewQueueHandler(sessSvc *service.SessionService, runs *run.Manager) *QueueHandler {
	return &QueueHandler{sessSvc: sessSvc, runs: runs}
}

// queueEntryResponse is one queued message as served to the frontend.
type queueEntryResponse struct {
	RunID      string                        `json:"run_id"`
	Content    string                        `json:"content"`
	Model      string                        `json:"model"`
	EnqueuedAt string                        `json:"enqueued_at"`
	Attachments []model.MessageAttachmentRef `json:"attachments,omitempty"`
}

// ListQueue handles GET /api/v1/sessions/:session_id/queue. Ownership is
// validated against the session row (404 without disclosing others'
// sessions); a queue-read failure returns the stored rows' best effort as an
// empty 200 (the queue is a transient structure — an unavailable read is a
// missing badge, not a failed page, matching the generating-flag family).
func (h *QueueHandler) ListQueue(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	sess, err := h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.FromContext(ctx).Error("queue read: session lookup failed",
			zap.String("op", "handler.list_queue"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "session lookup failed"))
		return
	}
	if sess == nil || sess.UserID != userID {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "session not found"))
		return
	}

	entries, err := h.runs.Store.QueueEntries(ctx, sessionID)
	if err != nil {
		// Degrade to an empty list: the queue is presentation-state, and the
		// authoritative content survives in the entries themselves.
		logger.FromContext(ctx).Warn("queue read failed; returning empty",
			zap.String("op", "handler.list_queue"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		entries = nil
	}

	out := make([]queueEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, queueEntryResponse{
			RunID:       e.RunID,
			Content:     e.Content,
			Model:       e.Model,
			EnqueuedAt:  e.EnqueuedAt.UTC().Format(time.RFC3339),
			Attachments: e.Attachments,
		})
	}
	c.JSON(http.StatusOK, gin.H{"queue": out})
}
