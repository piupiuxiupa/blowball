package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
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
	compSvc  *service.CompactionService
	orch     OrchestratorRunner
	// selection carries the model-catalog state (per-request-model-selection,
	// dual-axis form of model-effort-v2): it resolves EVERY request's
	// model/reasoning_effort pair into the turn's (model, effort) and fixes
	// the turn's compaction threshold and recorded model name. The zero value
	// carries no catalog and cannot resolve any request (test-only shape).
	selection ModelSelectionConfig
	// runs owns the turn-run lifecycle (turn-detach-resume): the Redis run
	// state (event log, meta, session claim) and the process-local registry
	// of running turns.
	runs    *run.Manager
	dataDir string
	newHub  func() *stream.Hub
	// writeRunEvents is the SSE subscription seam over the run event log —
	// the same implementation the resume endpoint uses. Tests may stub it.
	// turnDone is the owning turn's completion channel (the degraded-path
	// close signal when the store is unreachable).
	writeRunEvents func(ctx context.Context, w http.ResponseWriter, runs *run.Manager, runID, after string, headers map[string]string, turnDone <-chan struct{}) error
}

// NewMessageStreamHandler wires the streaming handler with its services, the
// orchestrator adapter, the run-lifecycle manager, and the dataDir used to
// resolve per-user workspace and skills roots. compSvc is the
// context-compaction service; it may be nil (or constructed with
// max_context_tokens 0), in which case every compaction check is skipped and
// turns behave exactly as before the capability. selection is the
// per-request-model-selection state (see ModelSelectionConfig); build it with
// NewModelSelectionConfig from the loaded config. The agent role (and the all
// role) constructs this; the api role does not.
func NewMessageStreamHandler(
	sessSvc *service.SessionService,
	msgSvc *service.MessageService,
	titleSvc *service.TitleService,
	compSvc *service.CompactionService,
	orch OrchestratorRunner,
	dataDir string,
	runs *run.Manager,
	selection ModelSelectionConfig,
) *MessageStreamHandler {
	h := &MessageStreamHandler{
		sessSvc:   sessSvc,
		msgSvc:    msgSvc,
		titleSvc:  titleSvc,
		compSvc:   compSvc,
		orch:      orch,
		runs:      runs,
		dataDir:   dataDir,
		selection: selection,
	}
	h.newHub = func() *stream.Hub { return stream.NewHub(stream.DefaultHubBufferSize) }
	h.writeRunEvents = writeRunEventStream
	return h
}

// sendMessageRequest is the JSON body for POST /api/v1/sessions/:session_id/messages.
type sendMessageRequest struct {
	Content string `json:"content"`
	// Model optionally selects the turn's model from the openai.models
	// catalog (per-request-model-selection); unknown names are rejected with
	// 400 INVALID_MODEL. Omitted → the default catalog entry.
	Model string `json:"model"`
	// ReasoningEffort optionally selects the turn's thinking level
	// (none|low|medium|high|xhigh|max). Omitted → the deployment default
	// (openai.default_reasoning_effort, itself defaulting to none). Invalid
	// values and non-"none" values on a thinking:false entry are rejected
	// with 400 INVALID_EFFORT; "none" is sent to the provider as a literal
	// value on thinking entries (B2 wire family) rather than meaning "omit
	// the parameter".
	ReasoningEffort string `json:"reasoning_effort"`
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
//  6. Claim the session's single active-run slot (Redis SET NX). A running
//     turn -> 409 SESSION_BUSY carrying the active run id.
//  7. Run the orchestrator on a TURN-scoped context held by the run registry
//     (turn-detach-resume): a client disconnect does NOT cancel the agent
//     loop; only the cancel endpoint, an error, or process shutdown does. The
//     runner streams events into a fresh hub whose single consumer (the
//     drainer) appends every event to the Redis run event log.
//  8. Concurrently, the SSE response subscribes to the run event log (the same
//     reader the resume endpoint uses), replaying from the beginning — so a
//     slow start never misses events and a disconnect only drops the
//     subscription, never the turn.
//  9. After the orchestrator returns (whenever that is — possibly long after
//     the client disconnected), persist the user message and the assistant
//     reply together in a single batch using a detached (background-derived,
//     trace_id-preserving) context, then finalize the run (terminal status,
//     session release, retain-window expiry).
//  10. If this was the first exchange, fire titleSvc.GenerateTitle in a
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

	// Per-request model selection (model-effort-v2 dual axes): resolve
	// model/reasoning_effort against the catalog into the turn config
	// injected into all three agents, plus the turn-level values every
	// downstream consumer uses — the compaction threshold (the resolved
	// entry's window) and the model name recorded in run meta / turn_usage.
	// Resolution ALWAYS produces a full selection; a parameter-less request
	// takes the default entry + deployment default effort.
	sel, errCode, errMsg := resolveModelSelection(h.selection, req.Model, req.ReasoningEffort)
	if errCode != "" {
		c.JSON(http.StatusBadRequest, errorBody(errCode, errMsg))
		return
	}
	turnModel := sel.Model
	turnLimit := sel.MaxContextTokens
	if turnLimit == 0 {
		// Safety net: a hand-built selection config without a window (tests)
		// falls back to the compaction service's own limit. Production
		// wiring derives both from the same mandatory catalog, so they agree.
		turnLimit = h.compSvc.FallbackMaxContext()
	}

	userID := middleware.UserIDFromCtx(c)
	sessionID := c.Param("session_id")
	tid := middleware.TraceIDFromCtx(c)
	// trace_id + session_id both ride the context so the raw-capture sink can
	// attribute every LLM call the orchestrator makes to this session.
	ctx := agent.WithSessionID(trace.WithContext(c.Request.Context(), tid), sessionID)

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

	agentMsgs, _, err := MessagesToAgentMessagesIndexed(prior)
	if err != nil {
		logger.L().Warn("reconstruct messages failed; falling back to current message only",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		agentMsgs = nil
	}

	// Context-compaction stitching (context-compaction capability). Two ways
	// the model context becomes the stitched form:
	//
	//  a) turn-start preventive compaction — the previous turn ended over the
	//     80% threshold (turn_usage.context_tokens), so compact NOW, before
	//     the first LLM call of this turn, saving a doomed full-window
	//     request. Only for non-first turns (a new session has no usage row).
	//  b) the session was compacted on an earlier turn
	//     (sessions.context_compacted) — stitch from the latest record.
	//
	// Both paths operate on the DURABLE history (drain + MySQL read): the
	// Redis-first view above may serve rows whose id is still 0 (ids are
	// minted by the MySQL flusher), and a boundary cursor must reference a
	// durable row identity. Both degrade to the full recovered history on any
	// miss (compaction skipped, record unavailable, boundary not locatable):
	// never block the turn.
	var activeRecord *model.ContextCompaction
	if h.compSvc.Enabled() {
		overThreshold := false
		lastContextTokens := 0
		if !isFirstTurn {
			tokens, terr := h.compSvc.LatestContextTokens(ctx, sessionID)
			if terr != nil {
				logger.L().Warn("turn-start context tokens read failed; skipping preventive compaction",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.Error(terr))
			} else {
				lastContextTokens = tokens
				overThreshold = h.compSvc.ShouldCompact(tokens, turnLimit)
			}
		}
		if overThreshold || sess.ContextCompacted {
			if durable, derr := h.compSvc.RecoverDurable(ctx, sessionID); derr == nil {
				durableMsgs, durableRow, rerr := MessagesToAgentMessagesIndexed(durable)
				if rerr != nil {
					logger.L().Warn("durable reconstruct failed; using full recovered history",
						zap.String("op", "handler.send_message"),
						zap.String("session_id", sessionID),
						zap.Error(rerr))
				} else {
					if overThreshold {
						activeRecord, _ = h.compSvc.Compact(ctx, service.CompactionInput{
							SessionID:     sessionID,
							UserID:        userID,
							TriggerKind:   model.CompactionTriggerTurnStart,
							TriggerTokens: lastContextTokens,
							Rows:          durable,
							AgentMsgs:     durableMsgs,
							LastRow:       durableRow,
							SummaryModel:  turnModel,
						})
					}
					if activeRecord == nil && sess.ContextCompacted {
						activeRecord = h.compSvc.LatestCompactionRecord(ctx, sessionID)
					}
					if activeRecord != nil {
						if stitched := stitchCompacted(durable, durableMsgs, activeRecord); stitched != nil {
							agentMsgs = stitched
						}
					}
				}
			} else {
				logger.L().Warn("durable recovery failed; using full recovered history",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.Error(derr))
			}
		}
	}

	messages := append(agentMsgs, agent.Message{Role: "user", Content: req.Content})

	// Capture the user message timestamp early so the persisted user row keeps
	// the request-arrival time even though persistence is deferred until after
	// the orchestrator succeeds.
	userMsgTime := time.Now().UTC()

	// Turn-scoped persistence bookkeeping shared by the mid-turn flush and the
	// turn-end save (context-compaction capability): how many merged events
	// have already been persisted determines the suffix the turn-end save
	// pushes, keeping both the MySQL rows (deterministic client_msg_ids) and
	// the Redis msgs:{sid} list free of duplicates.
	persist := turnPersistInfo{
		sessionID:   sessionID,
		userID:      userID,
		traceID:     tid,
		userContent: req.Content,
		userMsgTime: userMsgTime,
	}
	flushed := &turnFlushState{}

	// Mid-turn compaction seam: when compaction is configured, install a
	// between-rounds hook plus the synchronized event tap it flushes through.
	// Both are per-turn values handed to the orchestrator alongside the hub.
	var hooks TurnHooks
	if h.compSvc.Enabled() {
		tap := NewTurnEventTap()
		hooks = TurnHooks{Round: h.newRoundHook(tap, persist, flushed, turnLimit, turnModel), Tap: tap}
	}

	// ── turn-detach-resume: session claim, run registry, event log ──────────
	//
	// The turn runs on its OWN context, detached from the HTTP request: a
	// client disconnect no longer cancels generation. The run id is the
	// request's trace_id, delivered via the X-Run-Id response header and the
	// run_id meta stamped on every event in the log.

	// Claim the session's single active-run slot (Redis SET NX). A holder
	// means a turn is still running: reject with SESSION_BUSY and hand back
	// the run id so the client can attach instead of waiting blindly. A claim
	// TRANSPORT failure degrades to running unguarded (WARN) rather than
	// rejecting the chat: a Redis outage already puts persistence on its
	// direct-write fallback, and "messages always win" outranks the mutex —
	// the claim is best-effort mutual exclusion, not admission control.
	holder, claimed, err := h.runs.Store.ClaimSession(ctx, sessionID, tid)
	if err != nil {
		logger.L().Warn("session run claim failed; proceeding without mutual exclusion",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(err))
	} else if !claimed {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{
			"code":    "SESSION_BUSY",
			"message": "a turn is already running for this session",
			"run_id":  holder,
		}})
		return
	}

	// Run meta (ownership + status + the turn's resolved model) in Redis.
	// Best-effort: a failed write costs cancel/resume for this run, never the
	// turn itself.
	if err := h.runs.Store.InitMeta(ctx, tid, run.RunMeta{
		SessionID: sessionID,
		UserID:    userID,
		Status:    run.StatusRunning,
		Model:     turnModel,
		CreatedAt: userMsgTime.Format(time.RFC3339),
	}); err != nil {
		logger.L().Warn("run meta init failed",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	}

	// The turn context: detached, but carrying the same trace/session
	// attribution (raw capture, llm_raw_log) as the request context did.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turnCtx = agent.WithSessionID(trace.WithContext(turnCtx, tid), sessionID)
	regRun := h.runs.Registry.Register(tid, turnCancel)

	workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")
	skillsDir := filepath.Join(h.dataDir, userID, "skills")
	hub := h.newHub()

	// The drainer is the hub's single consumer: it appends every event to
	// run:{rid}:events (done included), heartbeats, and polls the
	// cross-process cancel flag. drained closes after the final drain, which
	// orders the terminal status write strictly after the last event.
	drainDone := run.StartDrainer(hub, h.runs.Store, h.runs.Registry, tid, run.HeartbeatEvery)

	type runResult struct {
		events []stream.StreamEvent
		usage  map[string]any
		err    error
	}
	resultCh := make(chan runResult, 1)

	go func() {
		// Registry teardown: Unregister last so Finish (which unblocks
		// WaitAll) happens while the entry still resolves.
		defer h.runs.Registry.Unregister(tid)
		defer regRun.Finish()
		events, usage, err := h.orch.Handle(turnCtx, workspaceRoot, skillsDir, userID, messages, hub, hooks, sel.override())
		hub.Close()
		// Wait for the drainer to flush the final events into the log BEFORE
		// flipping the run terminal: the status write must never precede the
		// last replayable event.
		<-drainDone

		// Terminal run bookkeeping (turn-detach-resume) runs ON THE TURN
		// goroutine, not the HTTP handler's: SSE subscribers (including this
		// request's own response) close their streams on the terminal status,
		// so it must be published without depending on the handler's own
		// progress. Status first (unblocks SSE readers), then the session
		// release (lifts SESSION_BUSY), then the retain-window expiry.
		turnStatus := run.StatusDone
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			turnStatus = run.StatusCancelled
		default:
			turnStatus = run.StatusError
		}
		finalizeCtx := agent.WithSessionID(trace.WithContext(context.Background(), tid), sessionID)
		h.runs.Finalize(finalizeCtx, tid, sessionID, turnStatus)
		turnCancel()

		resultCh <- runResult{events: events, usage: usage, err: err}
	}()

	// The SSE response is just another subscription to the run's event log:
	// it returns when the client disconnects (turn keeps running) or the run
	// reaches a terminal state (the subscriber loop observes the status — or,
	// on the degraded no-Redis path, the turn's own completion signal).
	if sseErr := h.writeRunEvents(c.Request.Context(), c.Writer, h.runs, tid, "0", map[string]string{"X-Run-Id": tid}, regRun.Done()); sseErr != nil && !errors.Is(sseErr, context.Canceled) {
		logger.L().Warn("sse write returned error",
			zap.String("op", "handler.send_message"),
			zap.String("session_id", sessionID),
			zap.Error(sseErr))
	}

	// Wait for the orchestrator to finish (success, error, or cancellation) so
	// the event stream collected by the adapter is complete. This can outlive
	// the HTTP connection by design: a disconnected client's handler goroutine
	// parks here until the detached turn completes.
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
	//
	// Mid-turn flush interaction (context-compaction capability): when a
	// mid-turn compaction flushed part of this turn already, only the
	// post-flush suffix is persisted — the deterministic client_msg_ids would
	// collapse the rows in MySQL, but the Redis msgs:{sid} read cache has no
	// such dedup, so the suffix split keeps the cache list exact.
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
			msgs, mErr := persist.buildTurnMessages(merged, flushed.count(), now)
			if mErr != nil {
				logger.L().Error("map event to message failed",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.Error(mErr))
				return
			}

			if len(msgs) > 0 {
				if err := h.sessSvc.SaveMessagesBatch(saveCtx, userID, msgs); err != nil {
					logger.L().Error("save event stream failed",
						zap.String("op", "handler.send_message"),
						zap.String("session_id", sessionID),
						zap.Error(err))
				}
			}

			// Persist per-agent token cost into turn_usage AFTER the message
			// batch. Failure is logged only — never roll back messages.
			if tu, ok := buildTurnUsage(sessionID, tid, userID, turnModel, usage); ok {
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
		// already have been streamed to the client. That includes an explicit
		// cancellation via the cancel endpoint or graceful shutdown
		// (context.Canceled — a client disconnect no longer cancels the turn,
		// see turn-detach-resume) AND upstream/transport/timeout failures such
		// as a model-provider 429 or 5xx, which can land mid-turn after
		// several rounds of assistant tokens/tool results. In both cases we
		// persist the partial event stream so the reloaded session history
		// matches what the user saw and the user's own message is never
		// silently lost. The persistEvents closure records the user message,
		// the merged assistant events, the turn's token cost into turn_usage
		// (the done event carries usage on the error path too), and fires
		// first-turn title generation from the partial assistant content.
		if errors.Is(res.err, context.Canceled) {
			logger.L().Warn("turn cancelled; persisting partial interrupted turn",
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
// fast per-session SUM() aggregation. The meta.context_tokens key (emitted by
// the agent layer from the turn's LAST LLM round) feeds ContextTokens for the
// turn-start preventive compaction check. turnModel (per-request-model-
// selection) is the turn's resolved model name recorded for per-model cost
// aggregation (empty → NULL). Returns ok=false when usage is nil or missing
// total.total_tokens, so the caller can skip persistence cleanly (e.g. a turn
// that errored before any LLM call produced usage).
func buildTurnUsage(sessionID, traceID, userID, turnModel string, usage map[string]any) (model.TurnUsage, bool) {
	if len(usage) == 0 {
		return model.TurnUsage{}, false
	}
	totalTokens := extractTotalTokens(usage)
	raw, err := json.Marshal(usage)
	if err != nil {
		return model.TurnUsage{}, false
	}
	return model.TurnUsage{
		SessionID:     sessionID,
		TraceID:       traceID,
		UserID:        userID,
		Model:         turnModel,
		UsageJSON:     string(raw),
		TotalTokens:   totalTokens,
		ContextTokens: extractContextTokens(usage),
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

// extractContextTokens pulls the turn's last-round context size out of the
// usage object's `meta` segment (usage.meta.context_tokens, emitted by the
// agent layer — see TurnBreakdown.LastRoundContextTokens). Tolerates the
// JSON-decoded map shape; 0 when absent (turns with no LLM call).
func extractContextTokens(usage map[string]any) int {
	metaRaw, ok := usage["meta"]
	if !ok {
		return 0
	}
	meta, ok := metaRaw.(map[string]any)
	if !ok {
		return 0
	}
	switch v := meta["context_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// turnPersistInfo bundles the per-turn identifiers the persistence path needs
// to render message batches (shared by the mid-turn flush and the turn-end
// save so both derive byte-identical rows — same client_msg_ids, same user
// message timestamp).
type turnPersistInfo struct {
	sessionID   string
	userID      string
	traceID     string
	userContent string
	userMsgTime time.Time
}

// buildTurnMessages renders the persistence batch for merged events
// [from:len(merged)] plus the user message when from == 0. Message ordinals
// are the merged-stream positions (user = 0, events 1..N), so an overlapping
// re-render produces the SAME deterministic client_msg_ids and the MySQL
// UNIQUE index collapses any redelivery.
func (p turnPersistInfo) buildTurnMessages(merged []stream.StreamEvent, from int, now time.Time) ([]model.Message, error) {
	if from > len(merged) {
		from = len(merged)
	}
	var msgs []model.Message
	if from == 0 {
		msgs = append(msgs, UserMessage(p.sessionID, p.traceID, p.userContent, p.userMsgTime))
	}
	for i := from; i < len(merged); i++ {
		msg, err := MessageFromEvent(merged[i], p.sessionID, p.traceID, i+1, now)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// turnFlushState tracks how much of the turn's merged event stream a mid-turn
// flush has already persisted. It is written by the flush (running on the
// orchestrator goroutine inside the round hook) and read by the turn-end save
// (running on the detached persistence goroutine), hence the mutex.
type turnFlushState struct {
	mu      sync.Mutex
	flushed int // number of merged events already persisted (0 = nothing flushed)
}

// mark records that the first n merged events are persisted. Monotonic: a
// stale lower value never wins.
func (f *turnFlushState) mark(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > f.flushed {
		f.flushed = n
	}
}

// count returns the number of merged events already persisted.
func (f *turnFlushState) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushed
}

// newRoundHook builds the mid-turn compaction hook (context-compaction
// capability, trigger=mid_turn): threshold check → flush-first persistence →
// recover+reconstruct → Compact → rewrite the in-memory round to the stitched
// context. Every failure inside is best-effort — WARN and return nil so the
// agent loop continues on the original conversation.
//
// limit and summaryModel are the TURN's per-request-model-selection values:
// the hook's threshold check compares against 0.8 × the request-selected
// model's window, and the checkpoint summary call runs on the selected model
// (captured here at turn start so the closure sees one consistent selection
// even if the surrounding request scope changes).
func (h *MessageStreamHandler) newRoundHook(tap *TurnEventTap, persist turnPersistInfo, flushed *turnFlushState, limit int, summaryModel string) agent.RoundHook {
	return func(ctx context.Context, lastUsage agent.Usage) []agent.Message {
		contextTokens := lastUsage.PromptTokens + lastUsage.CompletionTokens
		if !h.compSvc.ShouldCompact(contextTokens, limit) {
			return nil
		}
		log := logger.L().With(
			zap.String("op", "handler.round_hook"),
			zap.String("session_id", persist.sessionID),
			zap.Int("context_tokens", contextTokens),
		)

		// ① Flush-first: persist the turn so far through the ordinary batch
		// path. Compaction has a hard dependency on the flush — the boundary
		// cursor must point at real persisted rows — so a flush failure aborts
		// the compaction attempt.
		events := tap.Snapshot(ctx)
		if events == nil {
			log.Warn("mid-turn event snapshot unavailable; compaction skipped")
			return nil
		}
		merged := MergeEvents(events)
		msgs, err := persist.buildTurnMessages(merged, flushed.count(), time.Now().UTC())
		if err != nil {
			log.Warn("mid-turn flush batch build failed; compaction skipped", zap.Error(err))
			return nil
		}
		if len(msgs) > 0 {
			if err := h.sessSvc.SaveMessagesBatch(ctx, persist.userID, msgs); err != nil {
				log.Warn("mid-turn flush failed; compaction skipped", zap.Error(err))
				return nil
			}
		}
		flushed.mark(len(merged))

		// ② Recover the now-complete history from the DURABLE tier (bounded
		// drain + MySQL read): the flush just queued the turn's rows, and the
		// boundary cursor must reference real MySQL identities — the Redis
		// read cache would serve them with id 0. A drain failure aborts the
		// compaction attempt like a flush failure.
		rows, err := h.compSvc.RecoverDurable(ctx, persist.sessionID)
		if err != nil {
			log.Warn("mid-turn durable recover failed; compaction skipped", zap.Error(err))
			return nil
		}
		agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows)
		if err != nil {
			log.Warn("mid-turn reconstruct failed; compaction skipped", zap.Error(err))
			return nil
		}

		// ③ Compact the middle into a checkpoint.
		rec, ok := h.compSvc.Compact(ctx, service.CompactionInput{
			SessionID:     persist.sessionID,
			UserID:        persist.userID,
			TriggerKind:   model.CompactionTriggerMidTurn,
			TriggerTokens: contextTokens,
			Rows:          rows,
			AgentMsgs:     agentMsgs,
			LastRow:       lastRow,
			SummaryModel:  summaryModel,
		})
		if !ok {
			return nil
		}

		// ④ Rewrite the in-memory round to first + framed summary + retained
		// tail — the same shape later turns stitch from the record.
		stitched := stitchCompacted(rows, agentMsgs, rec)
		if stitched == nil {
			log.Warn("mid-turn stitch failed after compaction; keeping original context")
			return nil
		}
		return stitched
	}
}
