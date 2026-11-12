package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/artifact"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/store/fs"
	turnexec "github.com/lush/blowball/internal/turn"
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
	// artifacts is the turn-artifacts capability service (nil-safe): when
	// set, each turn end snapshots the turn's deliverables into the version
	// store and announces them via artifact events (see TurnHooks.FinalizeTurn).
	artifacts *artifact.Service
	// attachments is the image-attachment registry lookup (nil-safe): when
	// unset, requests carrying attachment_ids fail closed with 500 — only
	// hand-built tests that never send attachments can leave it nil.
	attachments attachmentStore
	// maxInputTokens caps the estimated token count of a user message
	// (add-input-token-limit), the config-resolved value of
	// messages.max_input_tokens. The estimate uses the CJK-aware heuristic in
	// tokens.go; a non-positive value skips the token check (the 1MB
	// request-body cap below stays in force regardless).
	maxInputTokens int
	turns          *turnexec.Executor
	// writeRunEvents is the SSE subscription seam over the run event log —
	// the same implementation the resume endpoint uses. Tests may stub it.
	// turnDone is the owning turn's completion channel (the degraded-path
	// close signal when the store is unreachable).
	writeRunEvents func(ctx context.Context, w http.ResponseWriter, runs *run.Manager, runID, after string, headers map[string]string, turnDone <-chan struct{}) error
	// queueMaxDepth is the session-message-queue knob (messages.queue): 0
	// keeps the queue disabled — busy sessions reject POSTs with 409
	// SESSION_BUSY and no pump/reconciler runs — while a positive value
	// enables queue admission with that depth cap. Set via
	// EnableMessageQueue at wiring time (kept out of the constructor so the
	// pre-queue call sites and tests stay byte-for-byte).
	queueMaxDepth int
	// shutdown stops new queue/background starts and cancels preparations.
	shutdown *turnexec.ShutdownGate
	// startupPumps tracks in-flight pump startups (pop → spawn). Shutdown
	// waits for them (WaitForStartupPumps) after tripping the gate: once a
	// startup's turn is SPAWNED it is registered and the registry's
	// CancelAll/WaitAll covers it — the WaitGroup covers exactly the window
	// the registry cannot see (review fix, round 3).
	startupPumps sync.WaitGroup
}

// EnableMessageQueue turns on the in-session message queue
// (session-message-queue, messages.queue.enabled) with the given depth cap.
// Kept out of the constructor so pre-queue call sites and tests stay
// unchanged: a handler that never calls this is byte-for-byte the 409
// SESSION_BUSY behavior. Non-positive maxDepth keeps the queue off.
func (h *MessageStreamHandler) EnableMessageQueue(maxDepth int) {
	if maxDepth > 0 {
		h.queueMaxDepth = maxDepth
	}
}

// StopNewTurns trips the process-level shutdown gate (idempotent): the
// queue pump tail and the reconciler stop popping new entries, in-flight
// pump STARTUPS are cancelled through shutdownCtx and return their entries
// to the queue, and — combined with WaitForStartupPumps — graceful shutdown
// never leaves a popped-but-unregistered turn behind that CancelAll cannot
// see (review fix, round 3). Entries stay enqueued for the post-restart
// reconciler.
func (h *MessageStreamHandler) StopNewTurns() {
	h.shutdown.Stop()
}

// newTurnsStopped reports whether the shutdown gate is tripped.
func (h *MessageStreamHandler) newTurnsStopped() bool {
	return h.shutdown.Stopped()
}

// WaitForStartupPumps waits (bounded) for every in-flight pump startup to
// settle — aborted ones have requeued their entries by the time it returns
// true. Returns false on timeout (a slow preparation may still requeue after
// the caller proceeds; the caller logs and exits at its own bound).
func (h *MessageStreamHandler) WaitForStartupPumps(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.startupPumps.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
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
// Optional scheduled services bind the headless scheduled entry point to its
// separate table group; omitting them keeps scheduled requests fail-closed.
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
	scheduled ...*turnexec.ScheduledServices,
) *MessageStreamHandler {
	var scheduledServices *turnexec.ScheduledServices
	if len(scheduled) > 0 {
		scheduledServices = scheduled[0]
	}
	gate := turnexec.NewShutdownGate()
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
		shutdown:       gate,
	}
	h.turns = turnexec.NewExecutor(turnexec.Dependencies{
		Sessions: sessSvc, Messages: msgSvc, Titles: titleSvc,
		Compaction: compSvc, Memory: memSvc, Orchestrator: orch,
		DataDir: dataDir, Runs: runs, Shutdown: gate,
		Scheduled: scheduledServices,
	})
	h.turns.SetQueuePump(h.pumpQueuedNext)
	h.writeRunEvents = writeRunEventStream
	return h
}

// TurnExecutor exposes the shared headless entry point to agent-role wiring.
func (h *MessageStreamHandler) TurnExecutor() *turnexec.Executor { return h.turns }

// WithArtifactService wires the turn-artifacts capability (nil disables it,
// preserving pre-capability behavior). Separate from the constructor so the
// many existing wiring/test call sites stay untouched.
func (h *MessageStreamHandler) WithArtifactService(s *artifact.Service) *MessageStreamHandler {
	h.artifacts = s
	h.turns.SetArtifactService(s)
	return h
}

// WithAttachmentStore wires the image-attachment registry
// (add-image-attachments): the send-time ownership validation and current-
// turn image loader read from it. Separate from the constructor so existing
// wiring/test call sites stay untouched.
func (h *MessageStreamHandler) WithAttachmentStore(store attachmentStore) *MessageStreamHandler {
	h.attachments = store
	return h
}

// WithRuntimeStatusStore enables Redis-only append-only status replay without
// changing the durable message services.
func (h *MessageStreamHandler) WithRuntimeStatusStore(store turnexec.RuntimeStatusStore) *MessageStreamHandler {
	h.turns.SetRuntimeStatusStore(store)
	return h
}

// validateSendAttachment performs the non-mutating lookup/ownership half of
// send-time attachment validation. Row-state semantics:
//   - unknown id / foreign owner    → 404 (no existence leak)
//   - bound to THIS session         → usable as-is (idempotent resend)
//   - bound to ANOTHER session      → 404
//   - staged (session_id NULL)      → eligible for the later claim phase
func (h *MessageStreamHandler) validateSendAttachment(c *gin.Context, ctx context.Context, userID, sessionID, id string) (*model.Attachment, bool) {
	att, err := h.attachments.GetAttachmentByUser(ctx, id, userID)
	if err != nil {
		logger.FromContext(ctx).Error("attachment lookup failed",
			zap.String("op", "handler.send_message"),
			zap.String("attachment_id", id),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "attachment lookup failed"))
		return nil, false
	}
	if att == nil {
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found: "+id))
		return nil, false
	}
	if att.SessionID != nil {
		if *att.SessionID == sessionID {
			return att, true
		}
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found: "+id))
		return nil, false
	}
	return att, true
}

// claimSendAttachment atomically claims and binds one pre-validated staged
// attachment. Bound-to-this-session rows return newlyClaimed=false.
func (h *MessageStreamHandler) claimSendAttachment(c *gin.Context, ctx context.Context, userID, sessionID string, att *model.Attachment) (bool, bool) {
	if att.SessionID != nil {
		return false, true
	}
	claimed, err := h.attachments.ClaimAttachment(ctx, att.ID, userID, sessionID)
	if err != nil {
		logger.FromContext(ctx).Error("attachment claim failed",
			zap.String("op", "handler.send_message"),
			zap.String("attachment_id", att.ID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "attachment claim failed"))
		return false, false
	}
	if !claimed {
		// Lost the race: another session claimed (or the sweeper reaped) the
		// staged row between lookup and claim — indistinguishable from a
		// foreign id, so the same 404 keeps the no-leak contract.
		c.JSON(http.StatusNotFound, errorBody("NOT_FOUND", "attachment not found: "+att.ID))
		return false, false
	}
	if !h.bindStagedFile(ctx, userID, sessionID, att) {
		return false, true
	}
	return true, true
}

// bindStagedFile moves a just-claimed attachment file from the staging
// sentinel directory into the claiming session's directory. The registry row
// already points at the session, so a rename failure never fails the turn:
// the claim is rolled back best-effort and the attachment degrades to the
// deterministic placeholder via the usual missing-file read path
// (loadCurrentTurnImages).
func (h *MessageStreamHandler) bindStagedFile(ctx context.Context, userID, sessionID string, att *model.Attachment) bool {
	ext, ok := attachmentMimeExt[att.MIME]
	if !ok {
		h.rollbackClaim(ctx, userID, sessionID, att.ID, fmt.Errorf("unknown attachment MIME %q", att.MIME))
		return false
	}
	src := filepath.Join(h.dataDir, userID, "attachments", fs.AttachmentStagingScope, att.ID+ext)
	dstDir := filepath.Join(h.dataDir, userID, "attachments", sessionID)
	dst := filepath.Join(dstDir, att.ID+ext)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		h.rollbackClaim(ctx, userID, sessionID, att.ID, err)
		return false
	}
	if err := os.Rename(src, dst); err != nil {
		h.rollbackClaim(ctx, userID, sessionID, att.ID, err)
		return false
	}
	return true
}

// rollbackClaim best-effort restores a just-claimed row to staged after the
// file bind failed, so the sweeper's TTL reclaims the orphaned staging file.
// A rollback failure leaves a bound row whose file lives under _staging —
// the read path degrades to the placeholder and the row follows the
// session-delete lifecycle.
func (h *MessageStreamHandler) rollbackClaim(ctx context.Context, userID, sessionID, id string, cause error) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	logger.FromContext(ctx).Error("staged attachment bind failed; rolling claim back",
		zap.String("op", "handler.send_message"),
		zap.String("attachment_id", id),
		zap.Error(cause))
	if err := h.attachments.UnclaimAttachment(rollbackCtx, id, userID, sessionID); err != nil {
		logger.FromContext(ctx).Error("attachment claim rollback failed",
			zap.String("op", "handler.send_message"),
			zap.String("attachment_id", id),
			zap.Error(err))
	}
}

// rollbackNewAttachmentClaims restores staged claims made by a request that is
// rejected before it owns the session run slot. Files move back to staging
// before the row is unclaimed, keeping the sweeper able to find the same retry
// unit if a later request never uses it.
func (h *MessageStreamHandler) rollbackNewAttachmentClaims(ctx context.Context, userID, sessionID string, ids []string) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ctx = rollbackCtx
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		att, err := h.attachments.GetAttachmentByUser(ctx, id, userID)
		if err != nil || att == nil || att.SessionID == nil || *att.SessionID != sessionID {
			continue
		}
		ext, ok := attachmentMimeExt[att.MIME]
		if !ok {
			logger.FromContext(ctx).Error("attachment rollback: unknown MIME; keeping row bound",
				zap.String("op", "handler.send_message"),
				zap.String("attachment_id", id))
			continue
		}
		renamed := false
		src := filepath.Join(h.dataDir, userID, "attachments", sessionID, id+ext)
		dstDir := filepath.Join(h.dataDir, userID, "attachments", fs.AttachmentStagingScope)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			logger.FromContext(ctx).Error("attachment rollback: create staging dir failed",
				zap.String("op", "handler.send_message"),
				zap.String("attachment_id", id), zap.Error(err))
			continue
		}
		if err := os.Rename(src, filepath.Join(dstDir, id+ext)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.FromContext(ctx).Error("attachment rollback: restore staging file failed",
					zap.String("op", "handler.send_message"),
					zap.String("attachment_id", id), zap.Error(err))
				// Keep the row bound rather than creating a staged row whose
				// file still lives under the session directory.
				continue
			}
		} else {
			renamed = true
		}
		if err := h.attachments.UnclaimAttachment(ctx, id, userID, sessionID); err != nil {
			logger.FromContext(ctx).Error("attachment rollback: unclaim failed",
				zap.String("op", "handler.send_message"),
				zap.String("attachment_id", id), zap.Error(err))
			if renamed {
				stagingPath := filepath.Join(h.dataDir, userID, "attachments", fs.AttachmentStagingScope, id+ext)
				sessionPath := filepath.Join(h.dataDir, userID, "attachments", sessionID, id+ext)
				if moveErr := os.Rename(stagingPath, sessionPath); moveErr != nil {
					logger.FromContext(ctx).Error("attachment rollback: compensate moved file failed",
						zap.String("op", "handler.send_message"),
						zap.String("attachment_id", id), zap.Error(moveErr))
				}
			}
		}
	}
}

// loadCurrentTurnImages reads the live turn's attachment files into in-memory
// payloads for the orchestrator (add-image-attachments). Reading happens once
// per turn on the handler goroutine; the OpenAI client re-encodes the bytes
// per call (deterministic for identical bytes). A file that cannot be read —
// deleted or corrupted underneath the registry — degrades to the same
// deterministic placeholder history uses, with an ERROR log: the turn must
// not fail because an old upload vanished.
func (h *MessageStreamHandler) loadCurrentTurnImages(
	ctx context.Context, userID, sessionID, content string, refs []model.MessageAttachmentRef,
) ([]agent.MessageImage, string) {
	images := make([]agent.MessageImage, 0, len(refs))
	var missing []string
	for _, ref := range refs {
		ext, ok := attachmentMimeExt[ref.MIME]
		if !ok {
			missing = append(missing, ref.Filename)
			continue
		}
		path := filepath.Join(h.dataDir, userID, "attachments", sessionID, ref.ID+ext)
		data, err := os.ReadFile(path)
		if err != nil {
			logger.FromContext(ctx).Error("current-turn attachment read failed; degrading to placeholder",
				zap.String("op", "handler.send_message"),
				zap.String("attachment_id", ref.ID),
				zap.String("path", path),
				zap.Error(err))
			missing = append(missing, ref.Filename)
			continue
		}
		images = append(images, agent.MessageImage{AttachmentID: ref.ID, Filename: ref.Filename, MIMEType: ref.MIME, Data: data})
	}
	return images, renderAttachmentPlaceholders(content, missing)
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
	// AttachmentIDs references images uploaded to the session's attachment
	// endpoint (add-image-attachments). Each id must resolve against the
	// attachments registry's (id, user, session) ownership triple, and the
	// turn's resolved model must carry the vision capability. Empty/omitted
	// is the pre-change plain-text request.
	AttachmentIDs []string `json:"attachment_ids"`
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
)

// incrementalPersistEvery is the cadence of the mid-turn round persister
// (refactor-message-storage): a polling window that picks up newly closed
// rounds. A var (not const) so tests can shrink it; production never touches
// it.
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
//     mismatched ownership -> 404. Validate + claim attachments (rollback on
//     any rejection below, until admission succeeds).
//  5. Admit the message to the session's run slot. Queue DISABLED
//     (messages.queue unset): claim the single active-run slot (Redis SET NX)
//     — a running turn -> 409 SESSION_BUSY carrying the active run id. Queue
//     ENABLED: one atomic claim-or-enqueue step — claim free AND queue empty
//     -> claim (fast path); otherwise append to the session queue (a
//     transport failure degrades to unguarded execution in both modes, the
//     standing "messages always win" convention); depth cap -> 409
//     QUEUE_FULL. An enqueued message gets its run meta (status queued) and
//     the `queued` event on the run log, its SSE response replays that event
//     and tails until the pump continues the turn, and this handler returns
//     without a turn goroutine (session-message-queue).
//  6. For an admitted (or degraded) message, startTurn (turn_start.go) runs
//     the shared turn pipeline: history recovery + compaction stitching +
//     memory recall (the preamble — after the claim, so a queued message
//     recovers history at pump time), the send-time title cadence trigger,
//     run meta, send-time user-row persistence, the incremental persister,
//     and the turn goroutine itself. The user row is persisted after every
//     history read of the turn (placement invariants at the write site), so
//     the turn is durable from that point; assistant events stay deferred to
//     the turn end so the first token is not delayed by a storage round-trip.
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
//     it already fired at send time (step 6). Queue enabled: after Finalize
//     the goroutine pumps the queued head as the next turn
//     (session-message-queue; skipped while shutting down).
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
	completionReserve := sel.MaxCompletionTokens
	requestBudget := agent.NewRequestBudget(turnLimit, completionReserve)
	if turnLimit > 0 && completionReserve > 0 && !requestBudget.Valid() {
		c.JSON(http.StatusInternalServerError, errorBody("INVALID_CONTEXT_BUDGET",
			fmt.Sprintf("model %q context window %d cannot accommodate completion quota %d",
				turnModel, turnLimit, completionReserve)))
		return
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
	// trace_id + session_id + user_id all ride the context so the raw-capture
	// sink can attribute every LLM call the orchestrator makes to this
	// session, and the per-user LLM client resolver (user-llm-token) can
	// pick the requesting user's credential for title generation and
	// turn-start compaction (Orchestrator.Handle re-injects the same value
	// for the turn itself).
	ctx := skillmarket.WithToken(agent.WithUserID(agent.WithSessionID(trace.WithContext(c.Request.Context(), tid), sessionID), userID), rawToken)

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

	// Attachment validation (add-image-attachments, relaxed by
	// add-staged-attachment-upload). Everything here happens BEFORE the
	// turn-slot claim and any persistence: an invalid reference never
	// occupies the run slot. Order: count cap → duplicate check → per-id
	// ownership validation → vision capability → staged claims. Ownership
	// (404) still outranks the model-capability rejection (400), while no
	// staged row mutates until every non-mutating check has passed.
	var userAttachments []model.MessageAttachmentRef
	var newlyClaimedAttachments []string
	claimsCommitted := false
	defer func() {
		if !claimsCommitted && len(newlyClaimedAttachments) > 0 {
			h.rollbackNewAttachmentClaims(ctx, userID, sessionID, newlyClaimedAttachments)
		}
	}()
	if len(req.AttachmentIDs) > 0 {
		if len(req.AttachmentIDs) > maxAttachmentsPerMessage {
			c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST",
				fmt.Sprintf("at most %d attachments are allowed per message", maxAttachmentsPerMessage)))
			return
		}
		if h.attachments == nil {
			logger.FromContext(ctx).Error("attachment store not wired on the streaming handler",
				zap.String("op", "handler.send_message"))
			c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "attachment store not configured"))
			return
		}
		seen := make(map[string]bool, len(req.AttachmentIDs))
		for _, id := range req.AttachmentIDs {
			if seen[id] {
				c.JSON(http.StatusBadRequest, errorBody("BAD_REQUEST", "duplicate attachment id "+id))
				return
			}
			seen[id] = true
		}
		validated := make([]*model.Attachment, 0, len(req.AttachmentIDs))
		for _, id := range req.AttachmentIDs {
			att, ok := h.validateSendAttachment(c, ctx, userID, sessionID, id)
			if !ok {
				return
			}
			validated = append(validated, att)
		}
		// Capability rejection must precede mutation: a non-vision request
		// must never consume a staged attachment.
		if !sel.Vision {
			c.JSON(http.StatusBadRequest, errorBody("MODEL_NOT_VISION",
				fmt.Sprintf("model %q does not support image attachments", turnModel)))
			return
		}
		for _, att := range validated {
			newlyClaimed, ok := h.claimSendAttachment(c, ctx, userID, sessionID, att)
			if !ok {
				return
			}
			userAttachments = append(userAttachments, model.MessageAttachmentRef{
				ID: att.ID, Filename: att.Filename, MIME: att.MIME,
			})
			if newlyClaimed {
				newlyClaimedAttachments = append(newlyClaimedAttachments, att.ID)
			}
		}
	}

	// ── admission: acquire (or queue behind) the session's run slot ─────────
	//
	// The turn-request form below is the shared start-ready shape: the
	// preamble (history recovery, compaction stitching, memory recall) runs
	// INSIDE startTurn — after the claim — so a queued message recovers
	// history at pump time (the previous turn's terminal batch is durable
	// before the claim is free, harden-turn-persistence D2) rather than at
	// enqueue time, and every entry path (HTTP fast path, degradation,
	// pump) produces the identical turn.
	//
	// userMsgTime is minted here for the HTTP paths (request time, the
	// pre-extraction behavior); the pump path mints its own at continuation
	// time — see turn_start.go's turnRequestFromQueueEntry for the ordering
	// rationale.
	now := time.Now()
	tr := &turnRequest{
		sessionID:        sessionID,
		userID:           userID,
		traceID:          tid,
		content:          req.Content,
		attachments:      userAttachments,
		sel:              sel,
		rawToken:         rawToken,
		userMsgTime:      now,
		contextCompacted: sess.ContextCompacted,
		metaCreatedAt:    now,
	}

	var started *startedTurn
	if h.queueMaxDepth > 0 {
		// session-message-queue admission (design D2): one atomic
		// claim-or-enqueue step. claim free AND queue empty → claim (the
		// fast path, byte-identical to a successful ClaimSession); busy or
		// queue non-empty → append at the tail; depth cap → QUEUE_FULL.
		// The entry carries the resolved snapshot, so the pumping process
		// never re-resolves the request (D1).
		entry := queueEntryFromRequest(tr, now)
		res, aerr := h.runs.Store.AdmitMessage(ctx, sessionID, entry, h.queueMaxDepth)
		if aerr != nil {
			// Admission transport failure: the queue itself is Redis, so
			// there is nothing to enqueue into — degrade exactly like the
			// legacy claim failure below (WARN + unguarded execution).
			// "messages always win" outranks the mutex.
			logger.FromContext(ctx).Warn("queue admission failed; proceeding without mutual exclusion",
				zap.String("op", "handler.send_message"),
				zap.Error(aerr))
			claimsCommitted = true
			started = h.startTurn(ctx, tr)
		} else {
			switch res.Outcome {
			case run.AdmitQueued:
				// The message waits behind the running turn. AdmitMessage
				// published its observable state ATOMICALLY with the RPUSH —
				// meta (status queued, so cancel and the events endpoint
				// answer it correctly) and the `queued` event on the run log
				// — so a pump that consumes the entry the instant it is
				// queued can never observe the initial state written late
				// (an unconditional follow-up InitMeta could overwrite
				// running or even done; review fix). The SSE response below
				// replays the queued event, then tails (possibly for a long
				// time) until the pump starts the turn; the user message row
				// is NOT written here — it lands at pump time inside
				// startTurn (design D4: an enqueue-time row would leak into
				// the running turn's mid-turn compaction recovery and would
				// sort before the previous turn's assistant rows).
				claimsCommitted = true
				// The SSE response is just another subscription to this
				// run's event log: it replays the queued event, then tails
				// (possibly for a long time) until the pump starts the turn
				// and events flow — or a cancel flips the status terminal
				// and the loop closes the stream. A client disconnect only
				// drops the subscription; the resume endpoint re-attaches
				// by the same run id. No resultCh wait exists here: the
				// turn goroutine is spawned later by the pump, not by this
				// request.
				if sseErr := h.writeRunEvents(ctx, c.Writer, h.runs, tid, "0",
					map[string]string{"X-Run-Id": tid}, nil); sseErr != nil && !errors.Is(sseErr, context.Canceled) {
					logger.FromContext(ctx).Warn("sse write returned error",
						zap.String("op", "handler.send_message"),
						zap.Error(sseErr))
				}
				return
			case run.AdmitFull:
				// Depth cap: reject with the queue analog of SESSION_BUSY.
				// The deferred rollback releases this request's staged
				// attachment claims (claimsCommitted is still false).
				c.JSON(http.StatusConflict, gin.H{"error": gin.H{
					"code":    "QUEUE_FULL",
					"message": fmt.Sprintf("the session queue is full (%d pending messages)", h.queueMaxDepth),
				}})
				return
			default: // run.AdmitClaimed — the fast path.
				claimsCommitted = true
				started = h.startTurn(ctx, tr)
			}
		}
	} else {
		// Legacy single-slot claim (queue disabled): byte-for-byte the
		// pre-capability behavior — including the 409 SESSION_BUSY body
		// carrying the active run id so the client can attach instead of
		// waiting blindly. A claim TRANSPORT failure degrades to running
		// unguarded (WARN) rather than rejecting the chat.
		holder, claimed, err := h.runs.Store.ClaimSession(ctx, sessionID, tid)
		if err != nil {
			logger.FromContext(ctx).Warn("session run claim failed; proceeding without mutual exclusion",
				zap.String("op", "handler.send_message"),
				zap.Error(err))
			claimsCommitted = true
			started = h.startTurn(ctx, tr)
		} else if !claimed {
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{
				"code":    "SESSION_BUSY",
				"message": "a turn is already running for this session",
				"run_id":  holder,
			}})
			return
		} else {
			// The request now owns the session run slot; validation failures
			// below no longer roll staged attachments back.
			claimsCommitted = true
			started = h.startTurn(ctx, tr)
		}
	}

	// The SSE response is just another subscription to the run's event log:
	// it returns when the client disconnects (turn keeps running) or the run
	// reaches a terminal state (the subscriber loop observes the status — or,
	// on the degraded no-Redis path, the turn's own completion signal). The
	// enriched request ctx (session_id + trace_id) is passed rather than the
	// bare request ctx so the subscription loop's logs correlate with the turn
	// (tool-call-log-correlation); cancellation semantics are identical.
	if sseErr := h.writeRunEvents(ctx, c.Writer, h.runs, tid, "0", map[string]string{"X-Run-Id": tid}, started.turnDone); sseErr != nil && !errors.Is(sseErr, context.Canceled) {
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
	<-started.resultCh
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
	sessionID       string
	userID          string
	traceID         string
	userContent     string
	userAttachments []model.MessageAttachmentRef
	userMsgTime     time.Time
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

// configuredInputBudget arms request budgeting only when the catalog entry
// provides both a context window and completion quota. Window size itself is
// not used as a production/test switch.
func configuredInputBudget(window, completion int) int {
	if window <= 0 || completion <= 0 {
		return 0
	}
	return agent.NewRequestBudget(window, completion).InputTokens
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
func (h *MessageStreamHandler) newRoundHook(tap *TurnEventTap, persist turnPersistInfo, flushed *turnFlushState, window, compactionBudget int, summaryModel string) agent.RoundHook {
	return func(ctx context.Context, lastUsage agent.Usage) []agent.Message {
		contextTokens := lastUsage.PromptTokens + lastUsage.CompletionTokens
		threshold := compactionBudget
		if threshold <= 0 {
			threshold = window
		}
		if !h.compSvc.ShouldCompact(contextTokens, threshold) {
			return nil
		}
		return h.compactMidTurn(ctx, tap, persist, flushed, window, compactionBudget, summaryModel, contextTokens, false, model.CompactionTriggerMidTurn)
	}
}

// compactMidTurn performs the flush-first compaction sequence shared by the
// pressure trigger and forced overflow recovery. force bypasses the threshold
// check but still requires a valid usable compaction candidate.
func (h *MessageStreamHandler) compactMidTurn(ctx context.Context, tap *TurnEventTap, persist turnPersistInfo, flushed *turnFlushState, window, inputBudget int, summaryModel string, contextTokens int, measured bool, triggerKind string) []agent.Message {
	log := logger.FromContext(ctx).With(
		zap.String("op", "handler.round_hook"),
		zap.Int("context_tokens", contextTokens),
		zap.Int("input_budget_tokens", inputBudget),
		zap.String("trigger_kind", triggerKind),
	)

	// ① Flush-first: persist the turn so far as round finals through the
	// transactional batch path. Compaction has a hard dependency on the
	// flush — the boundary cursor must point at real persisted rows — so
	// a flush failure aborts the compaction attempt. terminal=true closes
	// the in-flight round: this hook runs after its results landed.
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
	batch, consumed := persist.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(), time.Now().UTC(),
		roundRenderOptions{terminal: true})
	if len(batch.msgs) > 0 || len(batch.calls) > 0 {
		if err := h.sessSvc.SaveFinalMessages(ctx, persist.userID, batch.msgs, batch.calls); err != nil {
			flushed.persistMu.Unlock()
			log.Warn("mid-turn flush failed; compaction skipped", zap.Error(err))
			return nil
		}
		flushed.mark(consumed)
	}
	flushed.persistMu.Unlock()

	// ② Recover the now-complete history from the DURABLE tier (bounded
	// drain + MySQL read): the flush just queued the turn's rows, and the
	// boundary cursor must reference real MySQL identities — the Redis
	// read cache would serve them with id 0. A drain failure aborts the
	// compaction attempt like a flush failure.
	rows, calls, err := h.compSvc.RecoverDurable(ctx, persist.sessionID)
	if err != nil {
		log.Warn("mid-turn durable recover failed; compaction skipped", zap.Error(err))
		return nil
	}
	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(rows, calls)
	if err != nil {
		log.Warn("mid-turn reconstruct failed; compaction skipped", zap.Error(err))
		return nil
	}
	requestOverhead := 0
	if env, ok := agent.RequestEnvelopeFromContext(ctx); ok {
		requestOverhead = agent.EstimateRenderedRequest(env.System, nil, env.Tools).Tokens
	}
	requestOverhead += agent.EstimateCurrentStatusOverhead(time.Now())
	liveMessages := agent.LiveMessagesFromContext(ctx)

	// ③ Compact the middle into a checkpoint.
	rec, ok := h.compSvc.Compact(ctx, service.CompactionInput{
		SessionID:             persist.sessionID,
		UserID:                persist.userID,
		TriggerKind:           triggerKind,
		TriggerTokens:         contextTokens,
		Rows:                  rows,
		AgentMsgs:             agentMsgs,
		LastRow:               lastRow,
		SummaryModel:          summaryModel,
		InputBudget:           inputBudget,
		TriggerTokensMeasured: measured,
		RequestOverheadTokens: requestOverhead,
		LiveMessages:          liveMessages,
	})
	if !ok {
		return nil
	}

	// ④ Rewrite the in-memory round to first + framed summary + retained
	// tail — the same shape later turns stitch from the record. Apply the
	// live overlay here, against the durable reconstruction the compaction
	// plan measured, and mark this hook result so the agent's fallback restore
	// does not reinterpret its mutable round as a durable source.
	stitched := stitchCompacted(rows, calls, agentMsgs, rec)
	if stitched == nil {
		log.Warn("mid-turn stitch failed after compaction; keeping original context")
		return nil
	}
	final := agent.ApplyLiveOverlay(agentMsgs, stitched, liveMessages)
	agent.MarkLiveOverlayApplied(ctx)
	return applySummaryToRoundLive(final, rec)
}

// newBudgetedRoundHook adapts the compaction hook to both normal pressure
// checks and one forced recovery after a provider context overflow. The
// estimator is installed separately on the agent and passes appended messages
// into this closure's estimate.
func (h *MessageStreamHandler) newBudgetedRoundHook(tap *TurnEventTap, persist turnPersistInfo, flushed *turnFlushState, window, inputBudget int, summaryModel string) agent.RoundHook {
	base := h.newRoundHook(tap, persist, flushed, window, inputBudget, summaryModel)
	return func(ctx context.Context, lastUsage agent.Usage) []agent.Message {
		if force, forced := agent.ForcedCompactionFromContext(ctx); forced {
			// Explicit control-plane signal; zero provider usage is valid data
			// and must still follow the ordinary pressure policy below.
			triggerKind := model.CompactionTriggerMidTurn
			if strings.HasPrefix(force.Reason, "turn_start") {
				triggerKind = model.CompactionTriggerTurnStart
			}
			forceBudget := inputBudget
			if force.InputBudget > 0 {
				forceBudget = force.InputBudget
			}
			return h.compactMidTurn(ctx, tap, persist, flushed, window, forceBudget, summaryModel, force.EstimatedTokens, false, triggerKind)
		}
		return base(ctx, lastUsage)
	}
}

// startPersistHeartbeat arms the persist-phase heartbeat keeper
// (harden-turn-persistence D3). The drainer exits after its final drain, but
// the terminal persistence that follows may span seconds under retry — without
// a keeper, run:{rid}:alive (15s TTL) expires and the attach endpoint
// misclassifies the still-finalizing run as dead, force-clearing the claim
// mid-persist. The keeper beats at the drainer's cadence (compare-and-rearm
// semantics via Store.Heartbeat) until the returned stop function is called;
// stop is idempotent and blocks until the keeper goroutine has exited. The
// same helper also serves as the STARTUP keeper (session-message-queue):
// sustained beats between the pre-publish heartbeat and the drainer's first
// beat, so a slow send-time persist cannot outlive the alive TTL.
func startPersistHeartbeat(saveCtx context.Context, store run.Store, runID, sessionID string, every time.Duration) (stop func()) {
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	var once sync.Once
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
		once.Do(func() { close(hbStop) })
		<-hbDone
	}
}

// startIncrementalPersister arms the mid-turn round persister
// (refactor-message-storage): every incrementalPersistEvery it snapshots the
// turn's collected event stream through the TurnEventTap and persists CLOSED
// ROUNDS as final batches — a round closes when the next round's first token
// arrives, so the trailing (still-streaming) round never lands half-baked.
// The persisted range continues the shared flushed cursor: identical round
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
				flushed.persistMu.Lock()
				batch, consumed := persist.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(),
					time.Now().UTC(), roundRenderOptions{})
				if len(batch.msgs) == 0 && len(batch.calls) == 0 {
					flushed.persistMu.Unlock()
					continue
				}
				if err := h.sessSvc.SaveFinalMessages(saveCtx, persist.userID, batch.msgs, batch.calls); err != nil {
					flushed.persistMu.Unlock()
					log.Warn("round persist failed; will retry next tick",
						zap.Int("rows", len(batch.msgs)), zap.Error(err))
					continue
				}
				flushed.mark(consumed)
				flushed.persistMu.Unlock()
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}
