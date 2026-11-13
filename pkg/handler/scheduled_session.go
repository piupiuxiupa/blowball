package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/trace"
	"github.com/lush/blowball/pkg/service"
	"go.uber.org/zap"
)

// ScheduledSessionsService is the read-only scheduled-session surface.
type ScheduledSessionsService interface {
	List(ctx context.Context, userID, taskID string, offset, limit int) ([]service.ScheduledSessionSummary, error)
	GetDetail(ctx context.Context, userID, sessionID string) (*service.ScheduledSessionDetail, error)
	Messages(ctx context.Context, userID, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error)
	ToolCalls(ctx context.Context, userID, sessionID string) ([]model.ToolCall, error)
	Delete(ctx context.Context, userID, sessionID string) error
}

// ScheduledOccurrenceLookup is the Scheduler occurrence read used to enrich a
// task-session detail with its immutable historical snapshot.
type ScheduledOccurrenceLookup interface {
	GetOccurrence(ctx context.Context, req *schedulerv1.GetOccurrenceRequest) (*schedulerv1.GetOccurrenceResponse, error)
}

// ScheduledSessionHandler serves the task-session history endpoints. Task
// sessions are read-only: there is deliberately no send-message or manual
// re-run route.
type ScheduledSessionHandler struct {
	sessions     ScheduledSessionsService
	occurrences  ScheduledOccurrenceLookup
	defaultModel string
}

// NewScheduledSessionHandler wires the handler.
func NewScheduledSessionHandler(sessions ScheduledSessionsService, occurrences ScheduledOccurrenceLookup, defaultModel string) *ScheduledSessionHandler {
	return &ScheduledSessionHandler{sessions: sessions, occurrences: occurrences, defaultModel: defaultModel}
}

type scheduledSessionListEntry struct {
	SessionID        string    `json:"session_id"`
	Title            string    `json:"title"`
	TaskID           string    `json:"task_id"`
	OccurrenceID     string    `json:"occurrence_id"`
	RunID            string    `json:"run_id"`
	ContextCompacted bool      `json:"context_compacted"`
	SessionSource    string    `json:"session_source"`
	UpdateTime       time.Time `json:"update_time"`
	CreateTime       time.Time `json:"create_time"`
}

type scheduledSessionDetail struct {
	SessionID        string          `json:"session_id"`
	Title            string          `json:"title"`
	TaskID           string          `json:"task_id"`
	OccurrenceID     string          `json:"occurrence_id"`
	RunID            string          `json:"run_id"`
	ContextCompacted bool            `json:"context_compacted"`
	SessionSource    string          `json:"session_source"`
	UpdateTime       time.Time       `json:"update_time"`
	CreateTime       time.Time       `json:"create_time"`
	Snapshot         json.RawMessage `json:"snapshot,omitempty"`
	ActualModel      string          `json:"actual_model,omitempty"`
}

// List handles GET /api/v1/scheduled-sessions.
func (h *ScheduledSessionHandler) List(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	limit, offset, ok := scheduledTaskPagination(c)
	if !ok {
		return
	}
	rows, err := h.sessions.List(c.Request.Context(), userID, strings.TrimSpace(c.Query("task_id")), offset, limit)
	if err != nil {
		writeScheduledSessionError(c, err)
		return
	}
	entries := make([]scheduledSessionListEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, scheduledSessionListEntry{
			SessionID: r.SessionID, Title: r.Title, TaskID: r.TaskID,
			OccurrenceID: r.OccurrenceID, RunID: r.RunID,
			ContextCompacted: r.ContextCompacted,
			SessionSource:    string(r.Source),
			UpdateTime:       r.UpdateTime, CreateTime: r.CreateTime,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": entries, "limit": limit, "offset": offset})
}

// Get handles GET /api/v1/scheduled-sessions/:session_id. The immutable
// occurrence snapshot and the actual execution model are included when the
// Scheduler is reachable; a Scheduler failure degrades to the session row
// without the snapshot (WARN) rather than hiding the history.
func (h *ScheduledSessionHandler) Get(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	ctx := trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c))
	detail, err := h.sessions.GetDetail(ctx, userID, c.Param("session_id"))
	if err != nil {
		writeScheduledSessionError(c, err)
		return
	}
	if detail == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "scheduled session not found"))
		return
	}
	out := scheduledSessionDetail{
		SessionID: detail.Session.SessionID, Title: detail.Title,
		TaskID: detail.Session.TaskID, OccurrenceID: detail.Session.OccurrenceID,
		RunID: detail.Session.RunID, ContextCompacted: detail.Session.ContextCompacted,
		SessionSource: string(model.SessionSourceScheduled),
		UpdateTime:    detail.Session.UpdateTime, CreateTime: detail.Session.CreateTime,
	}
	h.enrichOccurrence(ctx, &out)
	c.JSON(http.StatusOK, out)
}

func (h *ScheduledSessionHandler) enrichOccurrence(ctx context.Context, out *scheduledSessionDetail) {
	if h.occurrences == nil || out.OccurrenceID == "" {
		return
	}
	resp, err := h.occurrences.GetOccurrence(ctx, &schedulerv1.GetOccurrenceRequest{OccurrenceId: out.OccurrenceID})
	if err != nil {
		logger.FromContext(ctx).Warn("scheduled session occurrence read failed; returning session without snapshot",
			zap.String("op", "handler.scheduled_session.detail"),
			zap.String("occurrence_id", out.OccurrenceID),
			zap.Error(err))
		return
	}
	o := resp.GetOccurrence()
	if o == nil {
		return
	}
	if raw := o.GetSnapshotJson(); json.Valid([]byte(raw)) {
		out.Snapshot = json.RawMessage(raw)
		var snap model.ScheduledTaskSnapshot
		if json.Unmarshal([]byte(raw), &snap) == nil {
			out.ActualModel = snap.Model
			if out.ActualModel == "" {
				out.ActualModel = h.defaultModel
			}
		}
	}
	if o.GetSessionSource() == schedulerv1.SessionSource_SESSION_SOURCE_LEGACY_INTERACTIVE {
		out.SessionSource = string(model.SessionSourceLegacyInteractive)
	}
}

// Messages handles GET /api/v1/scheduled-sessions/:session_id/messages.
func (h *ScheduledSessionHandler) Messages(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	ctx := trace.WithContext(c.Request.Context(), middleware.TraceIDFromCtx(c))
	sessionID := c.Param("session_id")

	var req getSessionMessagesRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	subagentContent := model.SubagentContentFull
	switch req.SubagentContent {
	case "", model.SubagentContentFull:
	case model.SubagentContentPlaceholder:
		subagentContent = model.SubagentContentPlaceholder
	default:
		c.JSON(http.StatusBadRequest, errorBody("INVALID_SUBAGENT_CONTENT", "subagent_content must be full or placeholder"))
		return
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	order := strings.ToLower(req.Order)
	if order != "desc" {
		order = "asc"
	}
	messages, nextCursor, err := h.sessions.Messages(ctx, userID, sessionID, req.PageToken, pageSize, order, subagentContent)
	if err != nil {
		writeScheduledSessionError(c, err)
		return
	}
	items := make([]sessionMessageItem, len(messages))
	needsCalls := false
	for i, m := range messages {
		items[i] = sessionMessageItem{Message: m}
		if m.ToolCallID != "" {
			needsCalls = true
		}
	}
	if needsCalls {
		if calls, cerr := h.sessions.ToolCalls(ctx, userID, sessionID); cerr != nil {
			logger.FromContext(ctx).Warn("list scheduled tool calls failed; rows returned without them",
				zap.String("op", "handler.scheduled_session.messages"),
				zap.String("session_id", sessionID),
				zap.Error(cerr))
		} else {
			byID := indexToolCallsByID(calls)
			for i := range items {
				if id := items[i].ToolCallID; id != "" {
					if call, ok := byID[items[i].TraceID+""+id]; ok {
						items[i].ToolCalls = []model.ToolCall{call}
					}
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"messages": items, "next_page_token": nextCursor})
}

// Delete handles DELETE /api/v1/scheduled-sessions/:session_id.
func (h *ScheduledSessionHandler) Delete(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, errorBody("UNAUTHORIZED", "authentication required"))
		return
	}
	if err := h.sessions.Delete(c.Request.Context(), userID, c.Param("session_id")); err != nil {
		writeScheduledSessionError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func writeScheduledSessionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrScheduledSessionNotFound):
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "scheduled session not found"))
	case errors.Is(err, service.ErrScheduledSessionActive):
		c.JSON(http.StatusConflict, errorBody("EXECUTION_STOP_NOT_PROVEN",
			"execution may still be running; cancel and obtain a stop proof before deleting"))
	default:
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "scheduled session operation failed"))
	}
}
