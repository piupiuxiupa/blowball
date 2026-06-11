package handler

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
)

// SessionHandler owns the /api/v1/sessions/* routes: SSE message streaming and
// the session list.
type SessionHandler struct {
	sessSvc  *service.SessionService
	msgSvc   *service.MessageService
	titleSvc *service.TitleService
	orch     OrchestratorRunner
	dataDir  string
	newHub   func() *stream.Hub
	writeSSE func(ctx context.Context, w http.ResponseWriter, h *stream.Hub) error
}

// NewSessionHandler wires the handler with its services, orchestrator adapter,
// and the dataDir used to resolve per-user workspace roots.
func NewSessionHandler(
	sessSvc *service.SessionService,
	msgSvc *service.MessageService,
	titleSvc *service.TitleService,
	orch OrchestratorRunner,
	dataDir string,
) *SessionHandler {
	h := &SessionHandler{
		sessSvc:  sessSvc,
		msgSvc:   msgSvc,
		titleSvc: titleSvc,
		orch:     orch,
		dataDir:  dataDir,
	}
	h.newHub = func() *stream.Hub { return stream.NewHub(stream.DefaultHubBufferSize) }
	h.writeSSE = stream.WriteSSE
	return h
}

// sendMessageRequest is the JSON body for POST /api/v1/sessions/:session_id/messages.
type sendMessageRequest struct {
	Content string `json:"content"`
}

// SendMessage handles POST /api/v1/sessions/:session_id/messages.
//
// Flow:
//  1. Parse body. Bad JSON / missing content -> 400.
//  2. Resolve user_id + session_id + workspace_root.
//  3. EnsureSession (creates the row + user dirs on first contact). Error -> 500.
//  4. Recover prior messages so we know whether this is the FIRST user turn
//     (title generation only fires on the first exchange).
//  5. Persist the user message BEFORE invoking the agent so a crash mid-stream
//     never loses it.
//  6. Run the orchestrator via OrchestratorRunner in a goroutine bound to the
//     request context (so a client disconnect cancels the agent loop). The
//     runner streams events into a fresh hub AND returns the final assistant
//     content.
//  7. Concurrently, stream.WriteSSE consumes from the same hub and writes the
//     SSE response.
//  8. After the orchestrator returns, persist the assistant reply using a
//     detached (background-derived, trace_id-preserving) context so a client
//     disconnect mid-stream does NOT lose the saved message.
//  9. If this was the first exchange, fire titleSvc.GenerateTitle in a
//     goroutine (fire-and-forget; never blocks the response).
func (h *SessionHandler) SendMessage(c *gin.Context) {
	var req sendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "content is required"))
		return
	}

	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	ctx := trace.WithContext(c.Request.Context(), tid)

	if err := h.sessSvc.EnsureSession(ctx, userID, sessionID); err != nil {
		logger.L().Error("session.ensure failed",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "session ensure failed"))
		return
	}

	prior, err := h.msgSvc.RecoverMessages(ctx, userID, sessionID)
	if err != nil {
		logger.L().Warn("recover messages failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		prior = nil
	}
	isFirstTurn := len(prior) == 0

	userMsg := model.Message{
		SessionID: sessionID,
		MsgTime:   time.Now().UTC(),
		Agent:     model.AgentConfuse,
		Role:      model.RoleUser,
		Content:   req.Content,
		TraceID:   tid,
	}
	if err := h.sessSvc.SaveMessage(ctx, userID, userMsg); err != nil {
		logger.L().Error("save user message failed",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "persist user message failed"))
		return
	}

	workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")

	hub := h.newHub()
	type runResult struct {
		assistantContent string
		err              error
	}
	resultCh := make(chan runResult, 1)

	go func() {
		// The orchestrator uses the request context so a client disconnect
		// cancels the agent loop. We close the hub when Handle returns so the
		// SSE writer drains remaining events and exits cleanly.
		defer hub.Close()
		content, err := h.orch.Handle(ctx, workspaceRoot, req.Content, hub)
		resultCh <- runResult{assistantContent: content, err: err}
	}()

	// writeSSE returns when the hub is closed (orchestrator finished) or the
	// request context is cancelled (client disconnect). Either way the HTTP
	// response is finished after this call returns.
	if sseErr := h.writeSSE(ctx, c.Writer, hub); sseErr != nil && !errors.Is(sseErr, context.Canceled) {
		logger.L().Warn("sse write returned error",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(sseErr))
	}

	// Persist the assistant reply using a detached context so a client
	// disconnect mid-stream (which cancels the request ctx) does NOT lose the
	// saved message. We always wait for the orchestrator to finish so the
	// content is complete.
	res := <-resultCh
	if res.err != nil && errors.Is(res.err, context.Canceled) && res.assistantContent == "" {
		// Client disconnect before the agent produced anything meaningful;
		// nothing worth persisting as assistant content. The user message is
		// already saved above.
		return
	}

	saveCtx := trace.WithContext(context.Background(), tid)
	assistantMsg := model.Message{
		SessionID: sessionID,
		MsgTime:   time.Now().UTC(),
		Agent:     model.AgentConfuse,
		Role:      model.RoleAssistant,
		Content:   res.assistantContent,
		TraceID:   tid,
	}
	if err := h.sessSvc.SaveMessage(saveCtx, userID, assistantMsg); err != nil {
		logger.L().Error("save assistant message failed",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
	}

	if isFirstTurn && h.titleSvc != nil {
		// Fire-and-forget; TitleService.GenerateTitle has its own recover().
		go h.titleSvc.GenerateTitle(saveCtx, sessionID, req.Content, res.assistantContent)
	}
}

// sessionListEntry is one element of the GET /api/v1/sessions response array.
type sessionListEntry struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	UpdateTime string `json:"update_time"`
}

// ListSessions handles GET /api/v1/sessions. Returns 200 with the user's
// sessions most-recently-updated first. An empty list returns 200 with
// {"sessions": []}.
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

	entries := make([]sessionListEntry, 0, len(sessions))
	for _, s := range sessions {
		entries = append(entries, sessionListEntry{
			SessionID:  s.SessionID,
			Title:      s.Title,
			UpdateTime: s.UpdateTime.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": entries})
}
