package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/stream"
)

// MessageStreamHandler owns the streaming message endpoint
// POST /api/v1/sessions/:session_id/messages. It is the only handler that
// couples the lightweight CRUD data plane to the heavy agent-execution path:
// session lookup + history recovery, an orchestrator run while writing SSE,
// three-layer turn persistence, and send-time title generation.
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
	// memSvc is the cross-session memory facade (cross-session-memory): nil
	// (or a disabled config) skips both the turn-start recall injection and
	// the turn-end capture — byte-for-byte pre-capability behavior.
	memSvc *memory.Service
	orch   OrchestratorRunner
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
	// maxInputTokens caps the estimated token count of a user message
	// (add-input-token-limit), the config-resolved value of
	// messages.max_input_tokens. The estimate uses the CJK-aware heuristic in
	// tokens.go; a non-positive value skips the token check (the 1MB
	// request-body cap below stays in force regardless).
	maxInputTokens int
	newHub         func() *stream.Hub
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
// turns behave exactly as before the capability. memSvc is the cross-session
// memory facade (cross-session-memory); it may be nil (disabled config), in
// which case recall injection and turn capture are both skipped. selection is
// the per-request-model-selection state (see ModelSelectionConfig); build it
// with NewModelSelectionConfig from the loaded config. maxInputTokens is the
// config-resolved user-input token cap (cfg.Messages.MaxInputTokensLimit();
// non-positive skips the check — pass 0 in tests that don't care). The agent
// role (and the all role) constructs this; the api role does not.
func NewMessageStreamHandler(
	sessSvc *service.SessionService,
	msgSvc *service.MessageService,
	titleSvc *service.TitleService,
	compSvc *service.CompactionService,
	memSvc *memory.Service,
	orch OrchestratorRunner,
	dataDir string,
	runs *run.Manager,
	selection ModelSelectionConfig,
	maxInputTokens int,
) *MessageStreamHandler {
	h := &MessageStreamHandler{
		sessSvc:        sessSvc,
		msgSvc:         msgSvc,
		titleSvc:       titleSvc,
		compSvc:        compSvc,
		memSvc:         memSvc,
		orch:           orch,
		runs:           runs,
		dataDir:        dataDir,
		selection:      selection,
		maxInputTokens: maxInputTokens,
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

// maxMessageBodyBytes caps the raw request body of the streaming message
// endpoint (add-input-token-limit, DoS backstop layer). 1MB leaves ~65×
// headroom over the worst-case 5000-token content (pure CJK ≈ 15KB), so a
// legitimate request can never hit it; it exists solely so a hostile body is
// rejected as it arrives instead of being buffered in full before parsing. It
// is deliberately a constant, not config — a backstop nobody should tune (the
// semantic cap lives in messages.max_input_tokens).
const maxMessageBodyBytes = 1 << 20

// Turn-end persistence retry bounds (harden-turn-persistence D4). Deliberately
// constants, not config — they are reliability backstops nobody should tune at
// runtime (mirroring internal/msgflush's errorBackoff/finalDrainTimeout). The
// budget bounds how long a dual-tier outage may delay the session claim
// release; attempts × backoff normally stays far below it.
const (
	// turnPersistBudget is the wall-clock ceiling for the turn-end message
	// batch, including retries. After it the batch is declared not durable
	// (ERROR) and Finalize proceeds so the session cannot lock up forever.
	turnPersistBudget = 30 * time.Second
	// turnPersistAttempts is the maximum number of SaveMessagesBatch calls
	// for one terminal batch; deterministic client_msg_ids make redelivery
	// idempotent.
	turnPersistAttempts = 3
	// turnPersistRetryBackoff parks between attempts so a dead dependency is
	// retried at a calm pace instead of hot-looping.
	turnPersistRetryBackoff = 1 * time.Second
	// turnEndDrainTimeout bounds the synchronous drain executed after the
	// terminal batch lands in Redis and before the session claim is released,
	// so the history endpoint (a pure MySQL reader) sees the complete turn as
	// soon as generating flips false. A drain failure only WARNs — the
	// background flusher closes the ≤1s residual gap.
	turnEndDrainTimeout = 5 * time.Second
)

// incrementalPersistEvery is the cadence of the mid-turn incremental
// persister (incremental-message-persistence). It deliberately matches the
// message flusher's default interval: pushing closed entries into msgs:buffer
// faster than the flusher drains them only grows the queue — MySQL visibility
// is gated by the flusher either way. A var (not const) so tests can shrink
// it; production never touches it.
var incrementalPersistEvery = 1 * time.Second

// SendMessage handles POST /api/v1/sessions/:session_id/messages.
//
// Flow:
//  1. Bound the request body (1MB DoS backstop), then parse it. An oversized
//     body -> 413; bad JSON / missing content -> 400.
//  2. Reject over-limit user input: content whose estimated token count
//     exceeds messages.max_input_tokens -> 400 CONTENT_TOO_LONG (add-input-
//     token-limit). Pure in-memory check, before any storage I/O.
//  3. Resolve user_id + session_id + workspace_root.
//  4. Validate that the session exists and belongs to the caller. Missing or
//     mismatched ownership -> 404.
//  5. Recover prior messages to build the agent context (and count prior user
//     rows for the title-generation cadence).
//  6. Capture the user message timestamp; the user row itself is persisted at
//     send time (step 8) so it is durable and visible for the whole turn,
//     while the assistant events stay deferred to the turn end so the first
//     token is not delayed by a storage round-trip.
//  7. Claim the session's single active-run slot (Redis SET NX). A running
//     turn -> 409 SESSION_BUSY carrying the active run id. Once claimed,
//     fire-and-forget title generation when the user-message ordinal hits the
//     cadence (n % 3 == 1) — in parallel with the turn, before it starts.
//  8. Persist the user message row synchronously through SaveMessagesBatch
//     (send-time-user-message-persistence): one Redis dual-write pipeline on
//     the handler goroutine, strictly after the claim and after every history
//     read of this turn (see the placement invariants at the write site). The
//     turn is durable from here on; a later batch re-including the row is
//     prevented by the turn's userPersisted flag.
//  9. Run the orchestrator on a TURN-scoped context held by the run registry
//     (turn-detach-resume): a client disconnect does NOT cancel the agent
//     loop; only the cancel endpoint, an error, or process shutdown does. The
//     runner streams events into a fresh hub whose single consumer (the
//     drainer) appends every event to the Redis run event log.
//  10. Concurrently, the SSE response subscribes to the run event log (the same
//     reader the resume endpoint uses), replaying from the beginning — so a
//     slow start never misses events and a disconnect only drops the
//     subscription, never the turn.
//  11. After the orchestrator returns (whenever that is — possibly long after
//     the client disconnected), the TURN goroutine persists the assistant
//     reply as an event-suffix batch using a detached (background-derived,
//     trace_id-preserving) context — synchronously, with bounded retry — then
//     runs a bounded drain so MySQL holds the batch, and only then finalizes
//     the run (terminal status, session release, retain-window expiry). The
//     session claim therefore outlives the terminal batch: generating:false
//     is a reliable "history complete" signal, and a process death can no
//     longer silently drop the batch (harden-turn-persistence). A heartbeat
//     keeper runs during the persist/drain phase so attach cannot misclassify
//     the still-finalizing run as dead. Title generation does NOT fire here —
//     it already fired at send time (step 7).
func (h *MessageStreamHandler) SendMessage(c *gin.Context) {
	// Cap the request body BEFORE JSON parsing so an oversized payload is
	// rejected as it arrives rather than buffered in full (same pattern as
	// the upload endpoint).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxMessageBodyBytes)

	var req sendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, errorBody("REQUEST_TOO_LARGE",
				"request body exceeds the 1MB limit"))
			return
		}
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", err.Error()))
		return
	}
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "content is required"))
		return
	}
	// User-input token cap (add-input-token-limit, semantic anti-abuse
	// layer). Conservative CJK-aware estimate — see estimateTokens. Rejected
	// here, before any store read or the session-run claim, so an abusive
	// input never occupies the turn slot or reaches the LLM. Non-positive
	// maxInputTokens (explicit config 0) skips the check; the byte cap above
	// stays in force regardless.
	if h.maxInputTokens > 0 {
		if est := estimateTokens(req.Content); est > h.maxInputTokens {
			c.JSON(http.StatusBadRequest, errorBody("CONTENT_TOO_LONG",
				fmt.Sprintf("content exceeds the %d-token limit (estimated %d tokens)",
					h.maxInputTokens, est)))
			return
		}
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
	// The RAW login JWT also rides the context (skill-market capability) so
	// the skillmarket client can authenticate the per-user allowlist fetch
	// from inside tool execution. AuthMiddleware verified the token but keeps
	// publishing only user_id — this is the single pass-through point, and it
	// reuses the verified header value rather than any stored copy.
	rawToken, _ := middleware.BearerToken(c.GetHeader("Authorization"))
	// trace_id + session_id both ride the context so the raw-capture sink can
	// attribute every LLM call the orchestrator makes to this session.
	ctx := skillmarket.WithToken(agent.WithSessionID(trace.WithContext(c.Request.Context(), tid), sessionID), rawToken)

	sess, err := h.sessSvc.GetSessionByID(ctx, sessionID)
	if err != nil {
		logger.FromContext(ctx).Error("session lookup failed",
			zap.String("op", "handler.send_message"),
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
		logger.FromContext(ctx).Warn("recover messages failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.Error(err))
		prior = nil
	}
	isFirstTurn := len(prior) == 0

	agentMsgs, _, err := MessagesToAgentMessagesIndexed(prior)
	if err != nil {
		logger.FromContext(ctx).Warn("reconstruct messages failed; falling back to current message only",
			zap.String("op", "handler.send_message"),
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
				logger.FromContext(ctx).Warn("turn-start context tokens read failed; skipping preventive compaction",
					zap.String("op", "handler.send_message"),
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
					logger.FromContext(ctx).Warn("durable reconstruct failed; using full recovered history",
						zap.String("op", "handler.send_message"),
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
				logger.FromContext(ctx).Warn("durable recovery failed; using full recovered history",
					zap.String("op", "handler.send_message"),
					zap.Error(derr))
			}
		}
	}

	// Cross-session memory recall (cross-session-memory capability): query the
	// user's OpenViking memory store with the new message and inject the
	// rendered block as an EPHEMERAL user-role message immediately before it
	// (adjacent user messages are already gateway-accepted — the compaction
	// stitched-summary precedent). Ephemeral means it is never persisted, never
	// part of RecoverMessages output, and never re-captured: it lives only in
	// this turn's LLM context. This runs on the request context BEFORE the
	// send-time user-row write below, satisfying the history-read invariant
	// documented there; any failure or timeout is a WARN + no injection —
	// memory must never block or fail a turn. (Known cost: recall precedes the
	// run claim, so a SESSION_BUSY-rejected request pays one wasted Find.)
	if h.memSvc.Enabled() {
		if block, err := h.memSvc.Recall(ctx, userID, req.Content); err != nil {
			logger.FromContext(ctx).Warn("memory recall failed; continuing without memory",
				zap.String("op", "handler.send_message"),
				zap.String("user_id", userID),
				zap.Error(err))
		} else if block != "" {
			agentMsgs = append(agentMsgs, agent.Message{Role: "user", Content: block})
		}
	}

	messages := append(agentMsgs, agent.Message{Role: "user", Content: req.Content})

	// Capture the user message timestamp early so the persisted user row keeps
	// the request-arrival time even though persistence is deferred until after
	// the orchestrator succeeds.
	userMsgTime := time.Now().UTC()

	// Turn-scoped persistence bookkeeping shared by the send-time user-row
	// write, the mid-turn flush, and the turn-end save (context-compaction +
	// send-time-user-message-persistence): how many merged events have already
	// been persisted determines the suffix the turn-end save pushes, and
	// whether the user row was persisted at send time gates its inclusion in
	// every later batch — keeping both the MySQL rows (deterministic
	// client_msg_ids) and the Redis msgs:{sid} list free of duplicates.
	persist := turnPersistInfo{
		sessionID:   sessionID,
		userID:      userID,
		traceID:     tid,
		userContent: req.Content,
		userMsgTime: userMsgTime,
	}
	flushed := &turnFlushState{}

	// The synchronized event tap is now UNCONDITIONAL (incremental-message-
	// persistence): the incremental persister below snapshots the collected
	// event stream through it every second, and the mid-turn compaction hook
	// (when compaction is configured) flushes through the same cursor state.
	// Both are per-turn values handed to the orchestrator alongside the hub.
	var hooks TurnHooks
	tap := NewTurnEventTap()
	if h.compSvc.Enabled() {
		hooks = TurnHooks{Round: h.newRoundHook(tap, persist, flushed, turnLimit, turnModel), Tap: tap}
	} else {
		hooks = TurnHooks{Tap: tap}
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
		logger.FromContext(ctx).Warn("session run claim failed; proceeding without mutual exclusion",
			zap.String("op", "handler.send_message"),
			zap.Error(err))
	} else if !claimed {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{
			"code":    "SESSION_BUSY",
			"message": "a turn is already running for this session",
			"run_id":  holder,
		}})
		return
	}

	// Title generation cadence (title-generation-cadence): fire-and-forget at
	// message-SEND time, in parallel with the turn. Placement — after the
	// session-run claim succeeded (a 409 SESSION_BUSY request returned above
	// and never reaches here; a claim transport failure degraded to unguarded
	// execution and still triggers, matching the persistence fallback
	// convention) and before the orchestrator starts — means the title lands
	// within seconds and survives turn errors/cancellation (GenerateTitle
	// derives its own detached context). Throttled by user-message ordinal:
	// n = prior persisted user rows + 1 (this message), trigger iff n % 3 == 1
	// — the 1st message always, then every 3rd (4th, 7th, ...). The
	// turn-serializing claim keeps the count deterministic: prior holds
	// exactly messages 1..n-1. Input is the session's first user message plus
	// the triggering message (both user-side anchors; on n=1 they coincide).
	if h.titleSvc != nil {
		userCount, firstUserMsg := 0, ""
		for _, m := range prior {
			if m.Role == model.RoleUser {
				if userCount == 0 {
					firstUserMsg = m.Content
				}
				userCount++
			}
		}
		if (userCount+1)%3 == 1 {
			if firstUserMsg == "" {
				firstUserMsg = req.Content
			}
			// Fire-and-forget; GenerateTitle has its own recover().
			go h.titleSvc.GenerateTitle(ctx, sessionID, firstUserMsg, req.Content)
		}
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
		logger.FromContext(ctx).Warn("run meta init failed",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	}

	// Send-time user-row persistence (send-time-user-message-persistence):
	// persist this turn's user message NOW — after the claim succeeded and
	// before the turn goroutine spawns — so the row is durable and visible to
	// history reads for the whole turn (a process crash can no longer lose it,
	// and a mid-turn refresh sees the question alongside the agent replay).
	// Placement invariants (design decision 1): the write MUST stay after
	// every history read of THIS turn — RecoverMessages and the turn-start
	// stitching/compaction block above captured `prior`/durable history before
	// the claim, and the LLM context appends the user message explicitly
	// (`messages`), so an earlier write would put the same message into the
	// recovered history AND the appended context — twice. It also stays after
	// the claim itself: 400/404/409 rejections above write nothing.
	//
	// The call is synchronous on the handler goroutine; the goroutine-spawn
	// happens-before edge below hands the set flag to the round hook and the
	// turn-end save without extra synchronization. On error the flag is NOT
	// set and the turn-end batch includes the user row as before
	// (at-least-once; the deterministic client_msg_id {trace_id}:0 collapses
	// the duplicate in MySQL). A Redis-pipeline failure never surfaces here —
	// SaveMessagesBatch degrades internally to a synchronous MySQL direct
	// write and returns nil.
	if err := h.sessSvc.SaveMessagesBatch(ctx, userID, []model.Message{
		UserMessage(persist.sessionID, persist.traceID, persist.userContent, persist.userMsgTime),
	}); err != nil {
		logger.FromContext(ctx).Warn("send-time user message persist failed; deferring the user row to the turn-end batch",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	} else {
		flushed.markUserPersisted()
	}

	// The turn context: detached, but carrying the same trace/session
	// attribution (raw capture, llm_raw_log) as the request context did — and
	// the login token, which flows from here into every tool execution ctx so
	// luban/executor market lookups survive the HTTP request's lifetime.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turnCtx = skillmarket.WithToken(agent.WithSessionID(trace.WithContext(turnCtx, tid), sessionID), rawToken)
	regRun := h.runs.Registry.Register(tid, turnCancel)

	workspaceRoot := filepath.Join(h.dataDir, userID, "workspace")
	skillsDir := filepath.Join(h.dataDir, userID, "skills")
	hub := h.newHub()

	// The drainer is the hub's single consumer: it appends every event to
	// run:{rid}:events (done included), heartbeats, and polls the
	// cross-process cancel flag. drained closes after the final drain, which
	// orders the terminal status write strictly after the last event.
	drainDone := run.StartDrainer(hub, h.runs.Store, h.runs.Registry, tid, sessionID, run.HeartbeatEvery)

	type runResult struct {
		events []stream.StreamEvent
		usage  map[string]any
		err    error
	}
	resultCh := make(chan runResult, 1)

	// saveCtx is a detached context that survives the HTTP request so the
	// turn-end persistence is not killed by a client disconnect. It carries
	// the turn's session_id + trace_id so its log sites (via logger.FromContext)
	// correlate with the turn (tool-call-log-correlation).
	saveCtx := agent.WithSessionID(trace.WithContext(context.Background(), tid), sessionID)

	// Mid-turn incremental persister (incremental-message-persistence): the
	// closed merged-event prefix lands every second while the turn runs; the
	// turn goroutine stops it (draining any in-flight batch) before the
	// terminal batch computes its suffix.
	stopIncremental := h.startIncrementalPersister(saveCtx, tap, persist, flushed)

	// persistTurnEvents writes the supplied assistant event stream as the
	// turn-end batch through the existing SaveMessagesBatch path and persists
	// the turn's per-agent cost into turn_usage. It runs SYNCHRONOUSLY on the
	// turn goroutine (harden-turn-persistence): the terminal batch must be
	// durable before Finalize releases the session claim, so a process death
	// can no longer silently drop it. The SaveMessagesBatch call retries
	// within a bounded budget (constants below) so a dual-tier outage delays
	// — rather than loses — the batch. The user row is included only when the
	// send-time write failed to land it (the userPersisted flag gates the
	// render — see send-time-user-message-persistence); on the ordinary path
	// this batch is a pure event suffix. turn_usage write failure is logged
	// but does NOT roll back the message batch (usage is observability data,
	// messages are business data — see the turn-cost-tracking spec's "Usage
	// write failure does not roll back messages" scenario). Title generation
	// is NOT triggered here — it fired at send time, before the turn started
	// (title-generation-cadence).
	//
	// Mid-turn flush interaction (context-compaction capability): when a
	// mid-turn compaction flushed part of this turn already, only the
	// post-flush suffix is persisted — the deterministic client_msg_ids would
	// collapse the rows in MySQL, but the Redis msgs:{sid} read cache has no
	// such dedup, so the suffix split keeps the cache list exact.
	persistTurnEvents := func(events []stream.StreamEvent, usage map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(saveCtx).Error("panic saving event stream",
					zap.String("op", "handler.send_message"),
					zap.Any("recover", r))
			}
		}()

		now := time.Now().UTC()
		merged := MergeEvents(events)

		// Cross-session memory capture (cross-session-memory capability):
		// fire-and-forget on BOTH terminal paths (this closure runs on the
		// error/cancel and the success path alike, exactly once each). Own
		// goroutine so a slow OpenViking round trip never delays the
		// message-batch write; own recover + WARN-only errors — the
		// TitleService fire-and-forget pattern. saveCtx is detached from the
		// HTTP request, so a client disconnect cannot kill the capture.
		// The submitted content is the persisted pair (persist.userContent
		// + top-level assistant tokens), never the ephemeral recall block.
		if h.memSvc.Enabled() {
			tc := memory.TurnCapture{
				UserID:        userID,
				SessionID:     sessionID,
				UserContent:   persist.userContent,
				AssistantText: topLevelAssistantText(merged),
				UserAt:        persist.userMsgTime,
				TurnEndAt:     now,
			}
			go func(tc memory.TurnCapture) {
				defer func() {
					if r := recover(); r != nil {
						logger.FromContext(saveCtx).Error("panic capturing turn to memory",
							zap.String("op", "handler.send_message"),
							zap.Any("recover", r))
					}
				}()
				if err := h.memSvc.CaptureTurn(saveCtx, tc); err != nil {
					logger.FromContext(saveCtx).Warn("memory capture failed; turn unaffected",
						zap.String("op", "handler.send_message"),
						zap.String("user_id", tc.UserID),
						zap.Error(err))
				}
			}(tc)
		}

		msgs, mErr := persist.buildTurnMessages(merged, flushed.count(), flushed.userPersisted(), now)
		if mErr != nil {
			logger.FromContext(saveCtx).Error("map event to message failed",
				zap.String("op", "handler.send_message"),
				zap.Error(mErr))
			return
		}

		if len(msgs) > 0 {
			// Bounded retry (harden-turn-persistence D4): the batch carries
			// deterministic client_msg_ids, so redelivery collapses to one
			// row per message — retrying the whole SaveMessagesBatch is
			// safe. A dual-tier outage delays the terminal batch (and with
			// it the claim release) by at most the budget; after the budget
			// it is an ERROR, not a silent loss.
			var lastErr error
			deadline := time.Now().Add(turnPersistBudget)
			for attempt := 1; attempt <= turnPersistAttempts; attempt++ {
				if err := h.sessSvc.SaveMessagesBatch(saveCtx, userID, msgs); err != nil {
					lastErr = err
					logger.FromContext(saveCtx).Warn("turn-end message persist attempt failed",
						zap.String("op", "handler.send_message"),
						zap.Int("attempt", attempt),
						zap.Int("rows", len(msgs)),
						zap.Error(err))
					if attempt == turnPersistAttempts || !time.Now().Add(turnPersistRetryBackoff).Before(deadline) {
						break
					}
					time.Sleep(turnPersistRetryBackoff)
					continue
				}
				lastErr = nil
				break
			}
			if lastErr != nil {
				logger.FromContext(saveCtx).Error("turn-end message persist budget exhausted; batch not durable",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.String("run_id", tid),
					zap.Int("rows", len(msgs)),
					zap.Error(lastErr))
			}
		}

		// Persist per-agent token cost into turn_usage AFTER the message
		// batch. Failure is logged only — never roll back messages.
		if tu, ok := buildTurnUsage(sessionID, tid, userID, turnModel, usage); ok {
			if err := h.sessSvc.SaveTurnUsage(saveCtx, tu); err != nil {
				logger.FromContext(saveCtx).Warn("save turn_usage failed; messages persisted",
					zap.String("op", "handler.send_message"),
					zap.Error(err))
			}
		}
	}

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

		// Turn outcome logging (moved from the handler side so it records at
		// the moment persistence starts): any orchestrator error interrupts
		// the turn after content may already have been streamed to the client.
		// That includes an explicit cancellation via the cancel endpoint or
		// graceful shutdown (context.Canceled — a client disconnect no longer
		// cancels the turn) AND upstream/transport/timeout failures such as a
		// model-provider 429 or 5xx. In every case the partial event stream is
		// persisted below so the reloaded session history matches what the
		// user saw and the user's own message is never silently lost.
		if err != nil {
			if errors.Is(err, context.Canceled) {
				logger.FromContext(saveCtx).Warn("turn cancelled; persisting partial interrupted turn",
					zap.String("op", "handler.send_message"),
					zap.String("user_id", userID),
					zap.Int("event_count", len(events)),
					zap.Error(err))
			} else {
				logger.FromContext(saveCtx).Error("orchestrator failed; persisting partial turn",
					zap.String("op", "handler.send_message"),
					zap.String("user_id", userID),
					zap.Int("event_count", len(events)),
					zap.Error(err))
			}
		}

		// Heartbeat keeper (harden-turn-persistence D3): the drainer has exited
		// (final drain done), but persistence below may span seconds under
		// retry. Keep run:{rid}:alive and the claim TTL armed so an attach
		// cannot misclassify this still-finalizing run as dead and force-clear
		// the claim mid-persist.
		stopHeartbeat := startPersistHeartbeat(saveCtx, h.runs.Store, tid, sessionID, run.HeartbeatEvery)

		// Stop the incremental persister FIRST (blocking until any in-flight
		// batch resolves) so the terminal batch below is exclusive and its
		// suffix starts at the final cursor value.
		stopIncremental()

		// Terminal persistence BEFORE Finalize (harden-turn-persistence D2):
		// the session claim is only released after this turn's message batch
		// is durable, making generating:false a reliable "history complete"
		// signal for clients.
		persistTurnEvents(events, usage)

		// Drain-before-release (D5): the terminal batch is in Redis
		// (msgs:{sid} + msgs:buffer); a bounded synchronous drain lands it in
		// MySQL before the claim flips so the history endpoint (a pure MySQL
		// reader) observes the complete turn the moment generating turns
		// false. A drain failure is a WARN, not a blocker — the background
		// flusher closes the ≤1s gap.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), turnEndDrainTimeout)
		if derr := h.sessSvc.DrainMessages(drainCtx); derr != nil {
			logger.FromContext(saveCtx).Warn("turn-end drain failed; flusher will catch up",
				zap.String("op", "handler.send_message"),
				zap.String("run_id", tid),
				zap.Error(derr))
		}
		drainCancel()
		stopHeartbeat()

		// Terminal run bookkeeping (turn-detach-resume) runs ON THE TURN
		// goroutine, not the HTTP handler's: SSE subscribers (including this
		// request's own response) close their streams on the terminal status,
		// so it must be published without depending on the handler's own
		// progress. Status first (unblocks SSE readers), then the session
		// release (lifts SESSION_BUSY), then the retain-window expiry — all
		// strictly after the terminal message batch is durable.
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
	// on the degraded no-Redis path, the turn's own completion signal). The
	// enriched request ctx (session_id + trace_id) is passed rather than the
	// bare request ctx so the subscription loop's logs correlate with the turn
	// (tool-call-log-correlation); cancellation semantics are identical.
	if sseErr := h.writeRunEvents(ctx, c.Writer, h.runs, tid, "0", map[string]string{"X-Run-Id": tid}, regRun.Done()); sseErr != nil && !errors.Is(sseErr, context.Canceled) {
		logger.FromContext(ctx).Warn("sse write returned error",
			zap.String("op", "handler.send_message"),
			zap.Error(sseErr))
	}

	// Wait for the orchestrator to finish (success, error, or cancellation) so
	// the event stream collected by the adapter is complete. This can outlive
	// the HTTP connection by design: a disconnected client's handler goroutine
	// parks here until the detached turn completes.
	// By the time the turn goroutine sends this value it has already
	// persisted the terminal message batch (with bounded retry) and run the
	// pre-release drain (harden-turn-persistence) — the handler side has no
	// persistence duty left.
	<-resultCh
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
// [from:len(merged)] plus the user message when from == 0 AND the user row was
// not already persisted at send time (send-time-user-message-persistence:
// callers pass the turn's userPersisted flag — the Redis msgs:{sid} list has
// no dedup, so a re-included user row would read back duplicated). Message
// ordinals are the merged-stream positions (user = 0, events 1..N), so an
// overlapping re-render produces the SAME deterministic client_msg_ids and the
// MySQL UNIQUE index collapses any redelivery.
func (p turnPersistInfo) buildTurnMessages(merged []stream.StreamEvent, from int, userPersisted bool, now time.Time) ([]model.Message, error) {
	if from > len(merged) {
		from = len(merged)
	}
	var msgs []model.Message
	if from == 0 && !userPersisted {
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
// flush has already persisted, plus whether the turn's user row was already
// persisted at send time (send-time-user-message-persistence). It is written
// by the send-time write (handler goroutine, strictly before the turn
// goroutine spawns) and the flush (orchestrator goroutine inside the round
// hook), and read by the turn-end save (detached persistence goroutine),
// hence the mutex.
type turnFlushState struct {
	mu      sync.Mutex
	flushed int  // number of merged events already persisted (0 = nothing flushed)
	userRow bool // the turn's user row was persisted at send time

	// persistMu serializes the "read cursor → persist batch → advance cursor"
	// critical section across the incremental persister goroutine and the
	// mid-turn compaction round hook (incremental-message-persistence). The
	// terminal batch runs strictly after the persister is stopped, so it is
	// naturally exclusive. Distinct from mu: state reads alone stay cheap and
	// lock-free of I/O ordering concerns.
	persistMu sync.Mutex
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

// markUserPersisted records that the turn's user message row was persisted at
// send time, so every later batch (mid-turn flush, turn-end save) must exclude
// it — the Redis msgs:{sid} list has no dedup, and the turn's user row must
// appear exactly once. Monotonic like mark: once set it never clears (a failed
// send-time write simply never sets it and the turn-end batch carries the row).
func (f *turnFlushState) markUserPersisted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userRow = true
}

// userPersisted reports whether the user row was already persisted.
func (f *turnFlushState) userPersisted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userRow
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
		log := logger.FromContext(ctx).With(
			zap.String("op", "handler.round_hook"),
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
		// Serialize the cursor critical section with the incremental
		// persister (incremental-message-persistence): both read the flushed
		// cursor, persist a non-overlapping range, then advance it.
		flushed.persistMu.Lock()
		msgs, err := persist.buildTurnMessages(merged, flushed.count(), flushed.userPersisted(), time.Now().UTC())
		if err != nil {
			flushed.persistMu.Unlock()
			log.Warn("mid-turn flush batch build failed; compaction skipped", zap.Error(err))
			return nil
		}
		if len(msgs) > 0 {
			if err := h.sessSvc.SaveMessagesBatch(ctx, persist.userID, msgs); err != nil {
				flushed.persistMu.Unlock()
				log.Warn("mid-turn flush failed; compaction skipped", zap.Error(err))
				return nil
			}
		}
		flushed.mark(len(merged))
		flushed.persistMu.Unlock()

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

// startPersistHeartbeat arms the persist-phase heartbeat keeper
// (harden-turn-persistence D3). The drainer exits after its final drain, but
// the terminal persistence that follows may span seconds under retry — without
// a keeper, run:{rid}:alive (15s TTL) expires and the attach endpoint
// misclassifies the still-finalizing run as dead, force-clearing the claim
// mid-persist. The keeper beats at the drainer's cadence (compare-and-rearm
// semantics via Store.Heartbeat) until the returned stop function is called;
// stop blocks until the keeper goroutine has exited.
func startPersistHeartbeat(saveCtx context.Context, store run.Store, runID, sessionID string, every time.Duration) (stop func()) {
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ticker.C:
				hbCtx, hbCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := store.Heartbeat(hbCtx, runID, sessionID); err != nil {
					logger.FromContext(saveCtx).Warn("persist-phase heartbeat failed",
						zap.String("op", "handler.send_message"),
						zap.String("run_id", runID),
						zap.Error(err))
				}
				hbCancel()
			}
		}
	}()
	return func() {
		close(hbStop)
		<-hbDone
	}
}

// startIncrementalPersister arms the mid-turn incremental persister
// (incremental-message-persistence): every incrementalPersistEvery it
// snapshots the turn's collected event stream through the (now unconditional)
// TurnEventTap and persists the CLOSED merged-event prefix — everything except
// the last merged entry, which may still be growing (MergeEvents only merges
// adjacent token/reasoning events, so every earlier entry is final). The
// persisted range continues the shared flushed cursor: identical merged
// ordinals, identical deterministic client_msg_ids as the terminal view, and
// strictly non-overlapping ranges — the msgs:{sid} read cache has no dedup.
//
// Failure policy: best-effort. A failed SaveMessagesBatch logs a WARN and does
// NOT advance the cursor; the next tick retries the same range and the
// terminal batch is the completeness backstop (it runs strictly after stop()).
// The persister therefore never blocks or fails the turn.
func (h *MessageStreamHandler) startIncrementalPersister(
	saveCtx context.Context,
	tap *TurnEventTap,
	persist turnPersistInfo,
	flushed *turnFlushState,
) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(incrementalPersistEvery)
		defer ticker.Stop()
		log := logger.FromContext(saveCtx).With(
			zap.String("op", "handler.incremental_persist"),
			zap.String("run_id", persist.traceID),
		)
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				// Bound the snapshot wait: a wedged collection goroutine must
				// never wedge the persister (and stop()) along with it.
				snapCtx, snapCancel := context.WithTimeout(saveCtx, incrementalPersistEvery)
				events := tap.Snapshot(snapCtx)
				snapCancel()
				if events == nil {
					continue // turn over (tap closed) or ctx done
				}
				merged := MergeEvents(events)
				if len(merged) < 2 {
					continue // nothing closed yet
				}
				closed := len(merged) - 1 // last entry may still grow

				flushed.persistMu.Lock()
				from := flushed.count()
				if closed <= from {
					flushed.persistMu.Unlock()
					continue
				}
				msgs, err := persist.buildTurnMessages(merged[:closed], from, flushed.userPersisted(), time.Now().UTC())
				if err != nil {
					flushed.persistMu.Unlock()
					log.Warn("incremental batch build failed; terminal batch will cover it", zap.Error(err))
					continue
				}
				if len(msgs) == 0 {
					flushed.persistMu.Unlock()
					continue
				}
				if err := h.sessSvc.SaveMessagesBatch(saveCtx, persist.userID, msgs); err != nil {
					flushed.persistMu.Unlock()
					log.Warn("incremental persist failed; will retry next tick",
						zap.Int("from", from), zap.Int("rows", len(msgs)), zap.Error(err))
					continue
				}
				flushed.mark(closed)
				flushed.persistMu.Unlock()
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}
