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
	"github.com/lush/blowball/internal/prompt"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/store/fs"
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

// WithArtifactService wires the turn-artifacts capability (nil disables it,
// preserving pre-capability behavior). Separate from the constructor so the
// many existing wiring/test call sites stay untouched.
func (h *MessageStreamHandler) WithArtifactService(s *artifact.Service) *MessageStreamHandler {
	h.artifacts = s
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
	logger.FromContext(ctx).Error("staged attachment bind failed; rolling claim back",
		zap.String("op", "handler.send_message"),
		zap.String("attachment_id", id),
		zap.Error(cause))
	if err := h.attachments.UnclaimAttachment(ctx, id, userID, sessionID); err != nil {
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
		images = append(images, agent.MessageImage{AttachmentID: ref.ID, MIMEType: ref.MIME, Data: data})
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
	completionReserve := sel.MaxCompletionTokens
	requestBudget := agent.NewRequestBudget(turnLimit, completionReserve)
	if turnLimit > 0 && completionReserve > 0 && !requestBudget.Valid() {
		c.JSON(http.StatusInternalServerError, errorBody("INVALID_CONTEXT_BUDGET",
			fmt.Sprintf("model %q context window %d cannot accommodate completion quota %d",
				turnModel, turnLimit, completionReserve)))
		return
	}
	statusSuffix := prompt.RenderStatus()

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

	prior, err := h.msgSvc.RecoverMessages(ctx, userID, sessionID)
	if err != nil {
		logger.FromContext(ctx).Warn("recover messages failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.Error(err))
		prior = nil
	}
	priorCalls, cerr := h.msgSvc.ListToolCalls(ctx, sessionID)
	if cerr != nil {
		logger.FromContext(ctx).Warn("recover tool calls failed; proceeding without",
			zap.String("op", "handler.send_message"),
			zap.Error(cerr))
		priorCalls = nil
	}

	agentMsgs, _, err := MessagesToAgentMessagesIndexed(prior, priorCalls)
	if err != nil {
		logger.FromContext(ctx).Warn("reconstruct messages failed; falling back to current message only",
			zap.String("op", "handler.send_message"),
			zap.Error(err))
		agentMsgs = nil
	}

	// Existing compacted sessions are stitched from the DURABLE tier. The
	// turn-start pressure check now runs inside the built agent just before its
	// first model call, where the exact system prompt, tools, current user
	// message, and budget are available.
	if h.compSvc.Enabled() && sess.ContextCompacted {
		if durable, durableCalls, derr := h.compSvc.RecoverDurable(ctx, sessionID); derr == nil {
			durableMsgs, _, rerr := MessagesToAgentMessagesIndexed(durable, durableCalls)
			if rerr != nil {
				logger.FromContext(ctx).Warn("durable reconstruct failed; using full recovered history",
					zap.String("op", "handler.send_message"),
					zap.Error(rerr))
			} else if activeRecord := h.compSvc.LatestCompactionRecord(ctx, sessionID); activeRecord != nil {
				if stitched := stitchCompacted(durable, durableCalls, durableMsgs, activeRecord); stitched != nil {
					agentMsgs = stitched
				}
			}
		} else {
			logger.FromContext(ctx).Warn("durable recovery failed; using full recovered history",
				zap.String("op", "handler.send_message"),
				zap.Error(derr))
		}
	}

	// Cross-session memory recall (cross-session-memory +
	// memory-recall-cache-stable): query the user's OpenViking memory store
	// with the new message and inject the NEW entries as a synthetic
	// recall_memories tool pair immediately before the user message. The
	// pair is persisted at send time (below) and replayed byte-identically
	// by history recovery, so turn payloads are append-only and the LLM
	// prefix cache covers prior turns; entries already injected this session
	// (derived from the recovered transcript) are never repeated, and a turn
	// with no new entries injects nothing at all. This runs on the request
	// context BEFORE the send-time user-row write below, satisfying the
	// history-read invariant documented there; any failure or timeout is a
	// WARN + no injection — memory must never block or fail a turn. (Known
	// cost: recall precedes the run claim, so a SESSION_BUSY-rejected
	// request pays one wasted Find.)
	var recall *recallInjection
	if h.memSvc.Enabled() {
		injected := injectedRecallKeys(agentMsgs)
		if res, err := h.memSvc.Recall(ctx, userID, req.Content, injected); err != nil {
			logger.FromContext(ctx).Warn("memory recall failed; continuing without memory",
				zap.String("op", "handler.send_message"),
				zap.String("user_id", userID),
				zap.Error(err))
		} else if res.Frame != "" {
			inj := newRecallInjection(sessionID, tid, req.Content, res)
			agentMsgs = append(agentMsgs, inj.agentMessages()...)
			recall = &inj
		}
	}
	req.Content += statusSuffix
	// Current-turn inline images (K=1 window): the LIVE user message carries
	// the image bytes read from disk; from the next turn on, reconstruction
	// renders this row as text placeholders instead. A missing file degrades
	// to the placeholder (ERROR log) — the turn must not die because a file
	// vanished underneath the registry.
	current := agent.Message{Role: "user", Content: req.Content}
	if len(userAttachments) > 0 {
		current.Images, current.Content = h.loadCurrentTurnImages(ctx, userID, sessionID, req.Content, userAttachments)
	}
	messages := append(agentMsgs, current)

	// Capture the user message timestamp early so the persisted user row keeps
	// the request-arrival time even though persistence is deferred until after
	// the orchestrator succeeds.
	userMsgTime := time.Now()

	// Turn-scoped persistence bookkeeping shared by the send-time user-row
	// write, the mid-turn flush, and the turn-end save (context-compaction +
	// send-time-user-message-persistence): how many merged events have already
	// been persisted determines the suffix the turn-end save pushes, and
	// whether the user row was persisted at send time gates its inclusion in
	// every later batch — keeping both the MySQL rows (deterministic
	// client_msg_ids) and the Redis msgs:{sid} list free of duplicates.
	persist := turnPersistInfo{
		sessionID:       sessionID,
		userID:          userID,
		traceID:         tid,
		userContent:     req.Content,
		userAttachments: userAttachments,
		userMsgTime:     userMsgTime,
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
		inputBudget := configuredInputBudget(turnLimit, sel.MaxCompletionTokens)
		hooks = TurnHooks{
			Round: h.newBudgetedRoundHook(tap, persist, flushed, turnLimit, inputBudget, turnModel),
			Estimate: func(_ context.Context, lastUsage agent.Usage, appended []agent.Message) agent.ContextRequestEstimate {
				return agent.EstimateRequestFromUsage(lastUsage.PromptTokens, lastUsage.CompletionTokens, appended)
			},
			BudgetedRound: func(ctx context.Context, _ agent.Usage, estimate agent.ContextRequestEstimate, system string, msgs []agent.Message, tools []byte) []agent.Message {
				if estimate.Tokens <= 0 || inputBudget <= 0 {
					return nil
				}
				// Only the preflight near-hard-budget path belongs here. The
				// ordinary 80% compression policy remains on Round above; firing
				// both for every measured round would summarize twice.
				if agent.EstimateRequestOverBudget(system, msgs, tools, inputBudget) {
					return h.compactMidTurn(ctx, tap, persist, flushed, turnLimit, inputBudget, turnModel, estimate.Tokens, true, model.CompactionTriggerMidTurn)
				}
				return nil
			},
			Tap: tap,
		}
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
	// The request now owns the session run slot; validation failures below no
	// longer roll staged attachments back.
	claimsCommitted = true

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
	// The synthetic recall pair (memory-recall-cache-stable) persists FIRST,
	// ahead of the user row, so both the Redis list order and the
	// (msg_time, msg_index) stream order match the payload order: pair rows
	// (msg_index -1) strictly before the user row (msg_index 0). One
	// AppendFinalBatch transaction lands the placeholder + tool_calls row;
	// the deterministic ids collapse any redelivery. Failure follows
	// memory's standing best-effort contract: WARN and keep the in-memory
	// injection (the model still sees its memories; the cost is a one-time
	// prefix break on the next turn, never a failed chat).
	if recall != nil {
		placeholder, call := recall.persistRows(persist.sessionID, persist.traceID, persist.userMsgTime)
		if err := h.sessSvc.SaveFinalMessages(ctx, userID, []model.Message{placeholder}, []model.ToolCall{call}); err != nil {
			logger.FromContext(ctx).Warn("send-time recall pair persist failed; continuing with in-memory injection only",
				zap.String("op", "handler.send_message"),
				zap.String("run_id", tid),
				zap.Error(err))
		}
	}
	if err := h.sessSvc.SaveMessagesBatch(ctx, userID, []model.Message{
		UserMessage(persist.sessionID, persist.traceID, persist.userContent, persist.userMsgTime, persist.userAttachments...),
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

	// Turn-artifacts capability: the turn-end finalization closure. The
	// adapter invokes it with this turn's top-level assistant text just
	// before the done event; linked paths are the artifact declaration.
	if h.artifacts != nil {
		svc := h.artifacts
		hooks.FinalizeTurn = func(ctx context.Context, assistantText string) []stream.ArtifactInfo {
			arts, err := svc.FinalizeTurn(ctx, userID, workspaceRoot, assistantText)
			if err != nil {
				logger.FromContext(ctx).Warn("artifact finalization failed; turn completes without artifact events",
					zap.String("op", "handler.artifact_finalize"),
					zap.String("user_id", userID),
					zap.Error(err))
				return nil
			}
			infos := make([]stream.ArtifactInfo, 0, len(arts))
			for _, a := range arts {
				infos = append(infos, stream.ArtifactInfo{
					Path:      a.Path,
					VersionID: a.VersionID,
					Size:      a.Size,
					Mime:      a.Mime,
					Op:        a.Op,
				})
			}
			return infos
		}
	}

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

	// persistTurnEvents renders the supplied assistant event stream as the
	// turn-end final batch (refactor-message-storage: round finals + tool
	// calls, one transaction) and persists
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
	persistTurnEvents := func(events []stream.StreamEvent, usage map[string]any, interrupted bool) {
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
		// + top-level assistant tokens); recall tool-pair rows are never
		// captured (capture sources only the user/assistant pair).
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

		batch, consumed := persist.buildRoundFinalBatch(merged, flushed.count(), flushed.userPersisted(), now,
			roundRenderOptions{terminal: true, interrupted: interrupted})

		if len(batch.msgs) > 0 || len(batch.calls) > 0 {
			// Bounded retry (harden-turn-persistence D4): the batch carries
			// deterministic client_msg_ids, so redelivery collapses to one
			// row per message — retrying the whole SaveMessagesBatch is
			// safe. A dual-tier outage delays the terminal batch (and with
			// it the claim release) by at most the budget; after the budget
			// it is an ERROR, not a silent loss.
			var lastErr error
			deadline := time.Now().Add(turnPersistBudget)
			for attempt := 1; attempt <= turnPersistAttempts; attempt++ {
				if err := h.sessSvc.SaveFinalMessages(saveCtx, userID, batch.msgs, batch.calls); err != nil {
					lastErr = err
					logger.FromContext(saveCtx).Warn("turn-end message persist attempt failed",
						zap.String("op", "handler.send_message"),
						zap.Int("attempt", attempt),
						zap.Int("rows", len(batch.msgs)),
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
					zap.Int("rows", len(batch.msgs)),
					zap.Error(lastErr))
			}
			flushed.mark(consumed)
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
		persistTurnEvents(events, usage, err != nil)

		// (Drain-before-release D5 is retired with the ingest queue: the
		// terminal batch above is a synchronous transactional MySQL write —
		// history is complete the moment it returns.)
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
	})
	if !ok {
		return nil
	}

	// ④ Rewrite the in-memory round to first + framed summary + retained
	// tail — the same shape later turns stitch from the record.
	stitched := stitchCompacted(rows, calls, agentMsgs, rec)
	if stitched == nil {
		log.Warn("mid-turn stitch failed after compaction; keeping original context")
		return nil
	}
	return applySummaryToRoundLive(stitched, rec)
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
