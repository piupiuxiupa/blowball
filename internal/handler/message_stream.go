package handler

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/stream"
)

// MessageStreamHandler owns the streaming message endpoint
// POST /api/v1/sessions/:session_id/messages. It is the only handler that
// couples the lightweight CRUD data plane to the heavy agent-execution path:
// session lookup + history recovery, an orchestrator run while writing SSE,
// three-layer turn persistence, and first-turn title generation.
//
// It is wired exclusively by the agent role (and the all role). The api role
// constructs SessionHandler only, so it never depends on the orchestrator or
// any other piece of the agent layer (see the service-roles spec's fault
// isolation requirement).
type MessageStreamHandler struct {
	sessSvc  *service.SessionService
	msgSvc   *service.MessageService
	titleSvc *service.TitleService
	orch     OrchestratorRunner
	dataDir  string
	newHub   func() *stream.Hub
	writeSSE func(ctx context.Context, w http.ResponseWriter, h *stream.Hub) error
}

// NewMessageStreamHandler wires the streaming handler with its services, the
// orchestrator adapter, and the dataDir used to resolve per-user workspace and
// skills roots. The agent role (and the all role) constructs this; the api role
// does not.
func NewMessageStreamHandler(
	sessSvc *service.SessionService,
	msgSvc *service.MessageService,
	titleSvc *service.TitleService,
	orch OrchestratorRunner,
	dataDir string,
) *MessageStreamHandler {
	h := &MessageStreamHandler{
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
//  3. Validate that the session exists and belongs to the caller. Missing or
//     mismatched ownership -> 404.
//  4. Recover prior messages so we know whether this is the FIRST user turn
//     (title generation only fires on the first exchange).
//  5. Capture the user message timestamp; the actual persistence happens later,
//     after the orchestrator succeeds, so the first token is not delayed by a
//     three-layer storage round-trip.
//  6. Run the orchestrator via OrchestratorRunner in a goroutine bound to the
//     request context (so a client disconnect cancels the agent loop). The
//     runner streams events into a fresh hub AND returns the final assistant
//     content.
//  7. Concurrently, stream.WriteSSE consumes from the same hub and writes the
//     SSE response.
//  8. After the orchestrator returns successfully, persist the user message and
//     the assistant reply together in a single batch using a detached
//     (background-derived, trace_id-preserving) context so a client disconnect
//     mid-stream does NOT lose the saved messages.
//  9. If this was the first exchange, fire titleSvc.GenerateTitle in a
//     goroutine (fire-and-forget; never blocks the response).
func (h *MessageStreamHandler) SendMessage(c *gin.Context) {
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

	sess, err := h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.L().Error("session lookup failed",
			zap.String("op", "handler.send_message"),
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

	prior, err := h.msgSvc.RecoverMessages(ctx, userID, sessionID)
	if err != nil {
		logger.L().Warn("recover messages failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		prior = nil
	}
	isFirstTurn := len(prior) == 0

	messages, err := MessagesToAgentMessages(prior)
	if err != nil {
		logger.L().Warn("reconstruct messages failed; falling back to current message only",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		messages = nil
	}
	messages = append(messages, agent.Message{Role: "user", Content: req.Content})

	// Capture the user message timestamp early so the persisted user row keeps
	// the request-arrival time even though persistence is deferred until after
	// the orchestrator succeeds.
	userMsgTime := time.Now().UTC()

	workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")
	skillsDir := filepath.Join(h.dataDir, userID, "skills")
	hub := h.newHub()
	type runResult struct {
		events []stream.StreamEvent
		err    error
	}
	resultCh := make(chan runResult, 1)

	go func() {
		// The orchestrator uses the request context so a client disconnect
		// cancels the agent loop. We close the hub when Handle returns so the
		// SSE writer drains remaining events and exits cleanly.
		defer hub.Close()
		events, err := h.orch.Handle(ctx, workspaceRoot, skillsDir, userID, messages, hub)
		resultCh <- runResult{events: events, err: err}
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

	// Wait for the orchestrator to finish (success, error, or cancellation) so
	// the event stream collected by the adapter is complete.
	res := <-resultCh

	// saveCtx is a detached context that survives the HTTP request so the
	// three-tier persistence goroutine is not killed by a client disconnect.
	saveCtx := trace.WithContext(context.Background(), tid)

	// persistEvents writes the user message and the supplied assistant event
	// stream using the existing SaveMessagesBatch path, then triggers title
	// generation for a first turn. It is used for both successful and
	// interrupted (client-canceled) turns.
	persistEvents := func(events []stream.StreamEvent) {
		// Title generation still needs a single assistant content string. We
		// derive it from the token events emitted by Confucius so the title
		// service contract remains unchanged.
		var assistantContent strings.Builder
		for _, e := range events {
			if e.Type == stream.EventToken && e.Agent == stream.AgentConfucius {
				assistantContent.WriteString(e.Content)
			}
		}

		go func(events []stream.StreamEvent) {
			defer func() {
				if r := recover(); r != nil {
					logger.L().Error("panic saving event stream",
						zap.String("op", "handler.send_message"),
						zap.String("session_id", sessionID),
						zap.Any("recover", r))
				}
			}()

			now := time.Now().UTC()
			merged := MergeEvents(events)
			msgs := make([]model.Message, 0, len(merged)+1)
			msgs = append(msgs, UserMessage(sessionID, tid, req.Content, userMsgTime))
			for i, e := range merged {
				msg, mErr := MessageFromEvent(e, sessionID, tid, i+1, now)
				if mErr != nil {
					logger.L().Error("map event to message failed",
						zap.String("op", "handler.send_message"),
						zap.String("session_id", sessionID),
						zap.Error(mErr))
					return
				}
				msgs = append(msgs, msg)
			}

			if err := h.sessSvc.SaveMessagesBatch(saveCtx, userID, msgs); err != nil {
				logger.L().Error("save event stream failed",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.Error(err))
			}
		}(events)

		if isFirstTurn && h.titleSvc != nil {
			// Fire-and-forget; TitleService.GenerateTitle has its own recover().
			go h.titleSvc.GenerateTitle(saveCtx, sessionID, req.Content, assistantContent.String())
		}
	}

	if res.err != nil {
		// Client-initiated cancellation means the user explicitly interrupted
		// the assistant turn. Persist the partial stream so the session can be
		// resumed from where it was cut off. Non-cancellation errors are still
		// transient/malformed and are discarded unchanged.
		if errors.Is(res.err, context.Canceled) {
			logger.L().Warn("client disconnected; persisting partial interrupted turn",
				zap.String("op", "handler.send_message"),
				zap.String("session_id", sessionID),
				zap.String("user_id", userID),
				zap.Int("event_count", len(res.events)),
				zap.Error(res.err))
			persistEvents(res.events)
			return
		}
		logger.L().Error("orchestrator failed",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.String("user_id", userID),
			zap.Error(res.err))
		return
	}

	// Success path: persist the full turn (user message + event stream) in one
	// asynchronous batch. The response has already been sent; there is no need
	// to block the HTTP handler on three-layer storage.
	persistEvents(res.events)
}
