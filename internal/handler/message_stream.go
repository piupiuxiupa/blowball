package handler

import (
	"context"
	"encoding/json"
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
		usage  map[string]any
		err    error
	}
	resultCh := make(chan runResult, 1)

	go func() {
		// The orchestrator uses the request context so a client disconnect
		// cancels the agent loop. We close the hub when Handle returns so the
		// SSE writer drains remaining events and exits cleanly.
		defer hub.Close()
		events, usage, err := h.orch.Handle(ctx, workspaceRoot, skillsDir, userID, messages, hub)
		resultCh <- runResult{events: events, usage: usage, err: err}
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
	// stream using the existing SaveMessagesBatch path, persists the turn's
	// per-agent cost into turn_usage, then triggers title generation for a first
	// turn. It is used for both successful and interrupted (client-canceled)
	// turns. turn_usage write failure is logged but does NOT roll back the
	// message batch (usage is observability data, messages are business data —
	// see the turn-cost-tracking spec's "Usage write failure does not roll back
	// messages" scenario).
	persistEvents := func(events []stream.StreamEvent, usage map[string]any) {
		// Title generation still needs a single assistant content string. We
		// derive it from the token events emitted by Confucius so the title
		// service contract remains unchanged.
		var assistantContent strings.Builder
		for _, e := range events {
			if e.Type == stream.EventToken && e.Agent == stream.AgentConfucius {
				assistantContent.WriteString(e.Content)
			}
		}

		go func(events []stream.StreamEvent, usage map[string]any) {
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

			// Persist per-agent token cost into turn_usage AFTER the message
			// batch. Failure is logged only — never roll back messages.
			if tu, ok := buildTurnUsage(sessionID, tid, userID, usage); ok {
				if err := h.sessSvc.SaveTurnUsage(saveCtx, tu); err != nil {
					logger.L().Warn("save turn_usage failed; messages persisted",
						zap.String("op", "handler.send_message"),
						zap.String("session_id", sessionID),
						zap.Error(err))
				}
			}
		}(events, usage)

		if isFirstTurn && h.titleSvc != nil {
			// Fire-and-forget; TitleService.GenerateTitle has its own recover().
			go h.titleSvc.GenerateTitle(saveCtx, sessionID, req.Content, assistantContent.String())
		}
	}

	if res.err != nil {
		// Any orchestrator error interrupts the turn after content may
		// already have been streamed to the client. That includes a client-
		// initiated cancellation (context.Canceled) AND upstream/transport/
		// timeout failures such as a model-provider 429 or 5xx, which can land
		// mid-turn after several rounds of assistant tokens/tool results. In
		// both cases we persist the partial event stream so the reloaded
		// session history matches what the user saw and the user's own message
		// is never silently lost. The persistEvents closure records the user
		// message, the merged assistant events, the turn's token cost into
		// turn_usage (the done event carries usage on the error path too), and
		// fires first-turn title generation from the partial assistant content.
		if errors.Is(res.err, context.Canceled) {
			logger.L().Warn("client disconnected; persisting partial interrupted turn",
				zap.String("op", "handler.send_message"),
				zap.String("session_id", sessionID),
				zap.String("user_id", userID),
				zap.Int("event_count", len(res.events)),
				zap.Error(res.err))
		} else {
			logger.L().Error("orchestrator failed; persisting partial turn",
				zap.String("op", "handler.send_message"),
				zap.String("session_id", sessionID),
				zap.String("user_id", userID),
				zap.Int("event_count", len(res.events)),
				zap.Error(res.err))
		}
		persistEvents(res.events, res.usage)
		return
	}

	// Success path: persist the full turn (user message + event stream) in one
	// asynchronous batch. The response has already been sent; there is no need
	// to block the HTTP handler on three-layer storage.
	persistEvents(res.events, res.usage)
}

// buildTurnUsage materializes a model.TurnUsage from the done event's usage
// object plus the request identifiers. The usage map is the authoritative
// {total, by_agent, meta} object; it is serialized verbatim into UsageJSON and
// its total.total_tokens is copied into the redundant TotalTokens column for
// fast per-session SUM() aggregation. Returns ok=false when usage is nil or
// missing total.total_tokens, so the caller can skip persistence cleanly
// (e.g. a turn that errored before any LLM call produced usage).
func buildTurnUsage(sessionID, traceID, userID string, usage map[string]any) (model.TurnUsage, bool) {
	if len(usage) == 0 {
		return model.TurnUsage{}, false
	}
	totalTokens := extractTotalTokens(usage)
	raw, err := json.Marshal(usage)
	if err != nil {
		return model.TurnUsage{}, false
	}
	return model.TurnUsage{
		SessionID:   sessionID,
		TraceID:     traceID,
		UserID:      userID,
		UsageJSON:   string(raw),
		TotalTokens: totalTokens,
	}, true
}

// extractTotalTokens pulls the aggregate total_tokens out of the usage object's
// nested `total` segment, tolerating the JSON-decoded map[string]any shape
// (numbers arrive as float64). Returns 0 when absent.
func extractTotalTokens(usage map[string]any) int {
	totalRaw, ok := usage["total"]
	if !ok {
		return 0
	}
	total, ok := totalRaw.(map[string]any)
	if !ok {
		return 0
	}
	switch v := total["total_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
