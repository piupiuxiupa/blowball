package handler

import (
	"errors"
	"net/http"
	"strings"
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

// SessionHandler owns the CRUD subset of the /api/v1/sessions/* routes:
// session list, create, message-history read, delete, and manual title update.
// The streaming message endpoint lives on MessageStreamHandler (agent role) so
// the api role never depends on the orchestrator or the rest of the agent layer.
type SessionHandler struct {
	sessSvc  *service.SessionService
	titleSvc *service.TitleService
	// runStore optionally reads the session active-run claims so the session
	// list can mark sessions with a running turn (turn-detach-resume). Nil
	// (e.g. unit tests) lists every session with generating=false.
	runStore run.Store
}

// NewSessionHandler wires the CRUD handler with its session service, the
// title service used for manual title updates, and optionally the run store
// backing the list's generating flag. It deliberately takes no orchestrator:
// the streaming concern is owned by MessageStreamHandler.
func NewSessionHandler(
	sessSvc *service.SessionService,
	titleSvc *service.TitleService,
	runStore run.Store,
) *SessionHandler {
	return &SessionHandler{
		sessSvc:  sessSvc,
		titleSvc: titleSvc,
		runStore: runStore,
	}
}

// updateTitleRequest is the JSON body for PATCH /api/v1/sessions/:session_id.
type updateTitleRequest struct {
	Title string `json:"title"`
}

// updateTitleResponse is the JSON body for PATCH /api/v1/sessions/:session_id.
type updateTitleResponse struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	UpdateTime string `json:"update_time"`
}

// createSessionResponse is the body for POST /api/v1/sessions.
type createSessionResponse struct {
	SessionID string `json:"session_id"`
}

// CreateSession handles POST /api/v1/sessions. The server mints a UUID v7
// session_id, persists the row, and returns it to the caller.
func (h *SessionHandler) CreateSession(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	sessionID, err := h.sessSvc.CreateSession(ctx, userID)
	if err != nil {
		logger.L().Error("create session failed",
			zap.String("op", "handler.create_session"),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "create session failed"))
		return
	}

	c.JSON(http.StatusOK, createSessionResponse{SessionID: sessionID})
}

// getSessionMessagesRequest captures the query parameters for
// GET /api/v1/sessions/:session_id/messages.
type getSessionMessagesRequest struct {
	PageToken       string `form:"page_token"`
	PageSize        int    `form:"page_size"`
	Order           string `form:"order"`
	SubagentContent string `form:"subagent_content"`
}

const (
	defaultPageSize = 50
	maxPageSize     = 5000
)

// getSessionMessagesResponse is the body for
// GET /api/v1/sessions/:session_id/messages.
type getSessionMessagesResponse struct {
	Messages      []model.Message `json:"messages"`
	NextPageToken string          `json:"next_page_token,omitempty"`
}

// GetSessionMessages handles GET /api/v1/sessions/:session_id/messages. It
// paginates the session's event stream from MySQL, validates ownership, and
// returns the canonical message rows.
func (h *SessionHandler) GetSessionMessages(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	var req getSessionMessagesRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}

	// Validate the sub-agent history view before any session/pagination read:
	// an unknown mode is a 400 with zero message-store I/O. An omitted value
	// defaults to the legacy full view (zero behavior change for old clients).
	subagentContent := model.SubagentContentFull
	switch req.SubagentContent {
	case "", model.SubagentContentFull:
		// full view
	case model.SubagentContentPlaceholder:
		subagentContent = model.SubagentContentPlaceholder
	default:
		c.JSON(http.StatusBadRequest, errorBody("INVALID_SUBAGENT_CONTENT", "subagent_content must be full or placeholder"))
		return
	}

	sess, err := h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.L().Error("session lookup failed",
			zap.String("op", "handler.get_session_messages"),
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

	messages, nextCursor, err := h.sessSvc.GetSessionMessages(ctx, sessionID, req.PageToken, pageSize, order, subagentContent)
	if err != nil {
		logger.L().Error("list session messages failed",
			zap.String("op", "handler.get_session_messages"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "list messages failed"))
		return
	}

	c.JSON(http.StatusOK, getSessionMessagesResponse{
		Messages:      messages,
		NextPageToken: nextCursor,
	})
}

// DeleteSession handles DELETE /api/v1/sessions/:session_id. Ownership is
// validated inside the service (missing or non-owned -> ErrSessionNotFound);
// success returns 204, not-found returns 404, any other failure returns 500.
func (h *SessionHandler) DeleteSession(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	if err := h.sessSvc.DeleteSession(ctx, userID, sessionID); err != nil {
		if errors.Is(err, service.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "session not found"))
			return
		}
		logger.L().Error("delete session failed",
			zap.String("op", "handler.delete_session"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "delete session failed"))
		return
	}

	c.Status(http.StatusNoContent)
}

// sessionListEntry is one element of the GET /api/v1/sessions response array.
type sessionListEntry struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	UpdateTime string `json:"update_time"`
	// Generating reports whether the session has a turn currently running
	// (turn-detach-resume): the Redis active-run claim exists. Clients use it
	// to offer attach/resume and a cancel affordance in the session list.
	Generating bool `json:"generating"`
	// RunID is the active turn's run id — the attach
	// (GET .../turns/{run_id}/events) and cancel target. Present only while
	// generating: it is THE reload discovery path, since a reloaded page has
	// lost the X-Run-Id header / event meta it saw when the turn started.
	RunID string `json:"run_id,omitempty"`
}

// ListSessions handles GET /api/v1/sessions. Returns 200 with the user's
// sessions most-recently-updated first. An empty list returns 200 with
// {"sessions": []}. Entries carry a generating flag derived from the Redis
// active-run claims; a claim-read failure degrades to generating=false for
// every entry (the list itself still succeeds).
func (h *SessionHandler) ListSessions(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	sessions, err := h.sessSvc.ListSessions(ctx, userID)
	if err != nil {
		logger.L().Error("list sessions failed",
			zap.String("op", "handler.list_sessions"),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "list sessions failed"))
		return
	}

	// Active-run claims: one batched read feeding both the generating flag
	// and the run_id (the reload discovery path) per entry.
	activeRuns := make(map[string]string, len(sessions))
	if h.runStore != nil {
		sids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			sids = append(sids, s.SessionID)
		}
		runs, err := h.runStore.ActiveRuns(ctx, sids)
		if err != nil {
			logger.L().Warn("active run lookup failed; listing without generating flags",
				zap.String("op", "handler.list_sessions"),
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			activeRuns = runs
		}
	}

	entries := make([]sessionListEntry, 0, len(sessions))
	for _, s := range sessions {
		rid := activeRuns[s.SessionID]
		entries = append(entries, sessionListEntry{
			SessionID:  s.SessionID,
			Title:      s.Title,
			UpdateTime: s.UpdateTime.UTC().Format(time.RFC3339),
			Generating: rid != "",
			RunID:      rid,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": entries})
}

// getSessionResponse is the body for GET /api/v1/sessions/:session_id — the
// session-list entry plus create_time (the single-session detail superset).
type getSessionResponse struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	CreateTime string `json:"create_time"`
	UpdateTime string `json:"update_time"`
	// Generating / RunID mirror the list entry: both derive from the Redis
	// active-run claim, and RunID is present only while generating (the
	// reload discovery path for attach/cancel).
	Generating bool   `json:"generating"`
	RunID      string `json:"run_id,omitempty"`
}

// GetSession handles GET /api/v1/sessions/:session_id. It returns the session
// detail for the owner; a missing or non-owned session returns 404 without
// disclosing existence. An active-run claim read failure degrades to
// generating=false (WARN), matching ListSessions. The title rides the detail
// read; a title lookup failure already degraded to an empty title there.
func (h *SessionHandler) GetSession(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	detail, err := h.sessSvc.GetSessionDetail(ctx, sessionID)
	if err != nil {
		logger.L().Error("session lookup failed",
			zap.String("op", "handler.get_session"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "session lookup failed"))
		return
	}
	if detail == nil || detail.Session.UserID != userID {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "session not found"))
		return
	}

	rid := ""
	if h.runStore != nil {
		runs, rerr := h.runStore.ActiveRuns(ctx, []string{sessionID})
		if rerr != nil {
			logger.L().Warn("active run lookup failed; returning generating=false",
				zap.String("op", "handler.get_session"),
				zap.String("session_id", sessionID),
				zap.String("user_id", userID),
				zap.Error(rerr))
		} else {
			rid = runs[sessionID]
		}
	}

	c.JSON(http.StatusOK, getSessionResponse{
		SessionID:  detail.Session.SessionID,
		Title:      detail.Title,
		CreateTime: detail.Session.CreateTime.UTC().Format(time.RFC3339),
		UpdateTime: detail.Session.UpdateTime.UTC().Format(time.RFC3339),
		Generating: rid != "",
		RunID:      rid,
	})
}

// UpdateTitle handles PATCH /api/v1/sessions/:session_id. It lets the session
// owner set a manual title. Manual titles are not overwritten by asynchronous
// AI title generation and the session's update_time is refreshed so the
// session appears first in the list.
func (h *SessionHandler) UpdateTitle(c *gin.Context) {
	var req updateTitleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "title is required"))
		return
	}

	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	sess, err := h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.L().Error("session lookup failed",
			zap.String("op", "handler.update_title"),
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

	sanitized, err := h.titleSvc.SetManualTitle(ctx, sessionID, req.Title)
	if err != nil {
		logger.L().Error("set manual title failed",
			zap.String("op", "handler.update_title"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "update title failed"))
		return
	}

	// Refresh the session row to obtain the touched update_time.
	sess, err = h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.L().Error("session re-lookup after title update failed",
			zap.String("op", "handler.update_title"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "update title failed"))
		return
	}
	if sess == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "session not found"))
		return
	}

	c.JSON(http.StatusOK, updateTitleResponse{
		SessionID:  sessionID,
		Title:      sanitized,
		UpdateTime: sess.UpdateTime.UTC().Format(time.RFC3339),
	})
}
