package handler

// turn-start extraction (session-message-queue): startTurn is the shared
// "run one message turn" pipeline. Three entry paths converge on it:
//
//   - the HTTP admission path (SendMessage): a session claim was just
//     acquired (queue disabled: ClaimSession; queue enabled: AdmitMessage's
//     claimed fast path, byte-identical to a successful ClaimSession);
//   - the admission degradation path (Redis unreachable): the turn runs
//     unguarded, exactly like the pre-queue claim-failure convention;
//   - the queue pump path (pumpQueuedNext / the reconciler): a queued entry
//     was popped and its claim transferred, and the run meta already exists
//     in the `queued` state from enqueue time.
//
// Everything between "the claim is held" and "the turn goroutine is running"
// lives here so the pumped turn is byte-for-byte the same turn an HTTP
// request would have started: history recovery + compaction stitching +
// memory recall (the preamble — deliberately INSIDE startTurn so a queued
// message recovers history at pump time, when the previous turn's terminal
// batch is durable, never at enqueue time), the title cadence trigger, run
// meta, send-time user-row persistence (whose placement invariants — after
// every history read of the turn, after the claim — are structurally
// satisfied by this ordering), the incremental persister, and the turn
// goroutine with its terminal persistence + Finalize + queue pump tail.

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/prompt"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/stream"
)

// runResult is what the turn goroutine reports to the HTTP wrapper: the
// collected event stream (for persistence bookkeeping already done on the
// goroutine itself), the usage object, and the orchestrator error. The
// channel is buffered so the goroutine never blocks on a disconnected
// client.
type runResult struct {
	events []stream.StreamEvent
	usage  map[string]any
	err    error
}

// startedTurn is the handle startTurn returns to its caller: the result
// channel the HTTP wrapper parks on after the SSE subscription ends, and the
// registry entry's completion channel (the degraded no-Redis close signal the
// SSE loop needs when the run store is unreachable). The pump path drops
// both.
type startedTurn struct {
	resultCh <-chan runResult
	turnDone <-chan struct{}
}

// turnRequest is the resolved, start-ready form of one message turn. The
// caller has already: validated the session ownership, validated + claimed
// attachments, resolved the model selection, and acquired (or degraded past)
// the session claim. userMsgTime is minted by the CALLER: request time for
// the HTTP paths, pump time for a queued entry (the ordering decision — a
// queued row dated at arrival would sort before the previous turn's
// assistant rows; see the session-message-queue design D4).
type turnRequest struct {
	sessionID string
	userID    string
	traceID   string
	// content is the RAW user content; the environment status suffix is
	// rendered inside startTurn so a pumped turn carries the pump-time
	// environment (and the persisted row matches the LLM context exactly).
	content     string
	attachments []model.MessageAttachmentRef
	sel         modelSelection
	rawToken    string
	userMsgTime time.Time
	// contextCompacted mirrors the session row's flag for the compaction
	// stitching branch (the HTTP path copies it from its session lookup; the
	// pump path re-reads the session row).
	contextCompacted bool
	// fromQueue marks a pumped queued entry: the run meta already exists in
	// the queued state (created at enqueue), so startTurn flips it to running
	// instead of initializing a fresh one.
	fromQueue bool
}

// startTurn runs the full turn pipeline for a request whose session claim is
// held (or whose admission degraded to unguarded execution). It returns after
// the turn goroutine is spawned; the HTTP wrapper then subscribes SSE and
// parks on the result channel. All preamble work (history recovery, recall)
// happens here — after the claim, satisfying the send-time persistence
// invariants — so every entry path produces the identical turn.
func (h *MessageStreamHandler) startTurn(ctx context.Context, req *turnRequest) *startedTurn {
	sessionID, userID, tid := req.sessionID, req.userID, req.traceID

	sel := req.sel
	turnModel := sel.Model
	turnLimit := sel.MaxContextTokens
	if turnLimit == 0 {
		// Safety net: a hand-built selection config without a window (tests)
		// falls back to the compaction service's own limit. Production
		// wiring derives both from the same mandatory catalog, so they agree.
		turnLimit = h.compSvc.FallbackMaxContext()
	}

	// ── preamble: history recovery, compaction stitching, memory recall ────
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
	if h.compSvc.Enabled() && req.contextCompacted {
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
	// with no new entries injects nothing at all. This runs on the turn
	// context BEFORE the send-time user-row write below, satisfying the
	// history-read invariant documented there; any failure or timeout is a
	// WARN + no injection — memory must never block or fail a turn. For a
	// queued (pumped) turn the query runs at pump time, when the previous
	// turn's answer is already in the recovered history.
	var recall *recallInjection
	if h.memSvc.Enabled() {
		injected := injectedRecallKeys(agentMsgs)
		if res, err := h.memSvc.Recall(ctx, userID, req.content, injected); err != nil {
			logger.FromContext(ctx).Warn("memory recall failed; continuing without memory",
				zap.String("op", "handler.send_message"),
				zap.String("user_id", userID),
				zap.Error(err))
		} else if res.Frame != "" {
			inj := newRecallInjection(sessionID, tid, req.content, res)
			agentMsgs = append(agentMsgs, inj.agentMessages()...)
			recall = &inj
		}
	}
	content := req.content + prompt.RenderStatus()
	// Current-turn inline images (K=1 window): the LIVE user message carries
	// the image bytes read from disk; from the next turn on, reconstruction
	// renders this row as text placeholders instead. A missing file degrades
	// to the placeholder (ERROR log) — the turn must not die because a file
	// vanished underneath the registry.
	current := agent.Message{Role: "user", Content: content}
	if len(req.attachments) > 0 {
		current.Images, current.Content = h.loadCurrentTurnImages(ctx, userID, sessionID, content, req.attachments)
	}
	messages := append(agentMsgs, current)

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
		userContent:     content,
		userAttachments: req.attachments,
		userMsgTime:     req.userMsgTime,
	}
	flushed := &turnFlushState{}

	// The synchronized event tap is UNCONDITIONAL (incremental-message-
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
			BudgetedRound: func(ctx context.Context, _ agent.Usage, estimate agent.ContextRequestEstimate, _ string, msgs []agent.Message, tools []byte) []agent.Message {
				if estimate.Tokens <= 0 || inputBudget <= 0 {
					return nil
				}
				// Only the preflight near-hard-budget path belongs here. The
				// ordinary 80% compression policy remains on Round above; firing
				// both for every measured round would summarize twice.
				// msgs already carries the system prompt; passing it again as a
				// separate envelope would double-count prompt tokens.
				if agent.EstimateRequestOverBudget("", msgs, tools, inputBudget) {
					return h.compactMidTurn(ctx, tap, persist, flushed, turnLimit, inputBudget, turnModel, estimate.Tokens, true, model.CompactionTriggerMidTurn)
				}
				return nil
			},
			Tap: tap,
		}
	} else {
		hooks = TurnHooks{Tap: tap}
	}

	// ── title cadence, run meta, send-time persistence ─────────────────────
	//
	// Title generation cadence (title-generation-cadence): fire-and-forget at
	// message-SEND time, in parallel with the turn. Placement — after the
	// session-run claim succeeded (a 409 SESSION_BUSY request returned above
	// and never reaches here; a claim transport failure degraded to unguarded
	// execution and still triggers, matching the persistence fallback
	// convention; a queued message triggers here at pump time) and before the
	// orchestrator starts — means the title lands within seconds and survives
	// turn errors/cancellation (GenerateTitle derives its own detached
	// context). Throttled by user-message ordinal: n = prior persisted user
	// rows + 1 (this message), trigger iff n % 3 == 1 — the 1st message
	// always, then every 3rd (4th, 7th, ...). The turn-serializing claim keeps
	// the count deterministic: prior holds exactly messages 1..n-1. Input is
	// the session's first user message plus the triggering message (both
	// user-side anchors; on n=1 they coincide).
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
				firstUserMsg = content
			}
			// Fire-and-forget; GenerateTitle has its own recover().
			go h.titleSvc.GenerateTitle(ctx, sessionID, firstUserMsg, content)
		}
	}

	// Run meta (ownership + status + the turn's resolved model) in Redis.
	// Best-effort: a failed write costs cancel/resume for this run, never the
	// turn itself. A pumped queued entry already has its meta (initialized
	// queued at enqueue); flip it to running so cancel/attach treat it as a
	// live run from this moment.
	if req.fromQueue {
		if err := h.runs.Store.SetStatus(ctx, tid, run.StatusRunning); err != nil {
			logger.FromContext(ctx).Warn("queued run status flip failed",
				zap.String("op", "handler.send_message"),
				zap.String("run_id", tid),
				zap.Error(err))
		}
	} else if err := h.runs.Store.InitMeta(ctx, tid, run.RunMeta{
		SessionID: sessionID,
		UserID:    userID,
		Status:    run.StatusRunning,
		Model:     turnModel,
		CreatedAt: req.userMsgTime.Format(time.RFC3339),
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
	// every history read of THIS turn — the preamble above captured `prior`
	// /durable history before this point, and the LLM context appends the
	// user message explicitly (`messages`), so an earlier write would put the
	// same message into the recovered history AND the appended context —
	// twice. It also stays after the claim itself: 400/404/409 rejections
	// above write nothing.
	//
	// The call is synchronous on the caller goroutine; the goroutine-spawn
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
	turnCtx = skillmarket.WithToken(agent.WithSessionID(trace.WithContext(turnCtx, tid), sessionID), req.rawToken)
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
	// within a bounded budget (constants in message_stream.go) so a dual-tier
	// outage delays — rather than loses — the batch. The user row is included
	// only when the send-time write failed to land it (the userPersisted flag
	// gates the render — see send-time-user-message-persistence); on the
	// ordinary path this batch is a pure event suffix. turn_usage write
	// failure is logged but does NOT roll back the message batch (usage is
	// observability data, messages are business data — see the
	// turn-cost-tracking spec's "Usage write failure does not roll back
	// messages" scenario). Title generation is NOT triggered here — it fired
	// at send time, before the turn started (title-generation-cadence).
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
		// signal for clients — and, for the queue, the guarantee the pumped
		// next turn's history recovery relies on.
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
		// release (lifts SESSION_BUSY / hands the slot to the queue pump),
		// then the retain-window expiry — all strictly after the terminal
		// message batch is durable.
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

		// Queue pump tail (session-message-queue): after the terminal
		// bookkeeping released the claim, try to continue the session's
		// queued head as the next turn. Skipped when the queue is disabled
		// or the process is shutting down (entries stay enqueued for the
		// post-restart reconciler).
		h.pumpQueuedNext(sessionID)
	}()

	return &startedTurn{resultCh: resultCh, turnDone: regRun.Done()}
}

// pumpQueuedNext continues a session's queued head after a turn's terminal
// bookkeeping (session-message-queue, design D5): pop the head with the
// atomic pop+claim transfer, rebuild the turn request from the entry
// snapshot, and start the turn through the same startTurn pipeline an HTTP
// request would use. A held claim (a racing admission won the slot) leaves
// the head enqueued; graceful shutdown skips the pump so the entry survives
// for the reconciler after restart.
func (h *MessageStreamHandler) pumpQueuedNext(sessionID string) {
	if h.queueMaxDepth <= 0 {
		return // queue disabled: byte-for-byte pre-capability behavior
	}
	if h.newTurnsStopped() {
		return // graceful shutdown: leave the entry to the post-restart reconciler
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = agent.WithSessionID(trace.WithContext(ctx, ""), sessionID)

	entry, ok, err := h.runs.Store.PumpHead(ctx, sessionID)
	if err != nil {
		logger.FromContext(ctx).Warn("queue pump: pop head failed; leaving the queue for the reconciler",
			zap.String("op", "handler.pump_queue"),
			zap.String("session_id", sessionID),
			zap.Error(err))
		return
	}
	if !ok {
		return // empty queue, or the claim is held by a racing admission
	}
	req, fatalErr := h.turnRequestFromQueueEntry(ctx, entry)
	if fatalErr != nil {
		// The entry cannot start (e.g. its session vanished between enqueue
		// and pump): finalize the run as error so the claim the pump just
		// transferred is released and waiting SSE subscribers close.
		logger.FromContext(ctx).Warn("queue pump: entry cannot start; finalizing as error",
			zap.String("op", "handler.pump_queue"),
			zap.String("run_id", entry.RunID),
			zap.Error(fatalErr))
		finCtx, finCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finCancel()
		h.runs.Finalize(finCtx, entry.RunID, entry.SessionID, run.StatusError)
		return
	}
	h.startTurn(ctx, req)
}

// turnRequestFromQueueEntry rebuilds a start-ready turn request from a
// queued entry snapshot (design D1: the snapshot — not the raw request
// parameters — is what travels, so the pumping process never re-resolves
// the request). userMsgTime is minted HERE (pump time): the ordering
// decision from design D4 — a queued row dated at arrival would sort before
// the previous turn's assistant rows in (msg_time, msg_index) history
// ordering.
func (h *MessageStreamHandler) turnRequestFromQueueEntry(ctx context.Context, entry run.QueueEntry) (*turnRequest, error) {
	// Re-validate the session survived until pump time. The enqueue path
	// verified ownership; DeleteSession clears the queue, so a miss here is
	// the tiny delete-vs-pump race — answer it by refusing to start.
	sess, err := h.sessSvc.GetSessionByID(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.UserID != entry.UserID {
		return nil, errors.New("session no longer exists or no longer belongs to the queueing user")
	}
	return &turnRequest{
		sessionID:        entry.SessionID,
		userID:           entry.UserID,
		traceID:          entry.RunID,
		content:          entry.Content,
		attachments:      entry.Attachments,
		sel:              selectionFromQueue(entry.Selection),
		rawToken:         entry.RawToken,
		userMsgTime:      time.Now(),
		contextCompacted: sess.ContextCompacted,
		fromQueue:        true,
	}, nil
}

// queueEntryFromRequest snapshots an admission-validated request into the
// queue's durable form (design D1): the resolved selection, the attachment
// references (plus which ones this request newly claimed from staging, for
// the cancel rollback), the login JWT the pumped turn's tool executions
// will need, and the arrival time (display only — the persisted user row
// takes pump time, see turnRequestFromQueueEntry).
func queueEntryFromRequest(req *turnRequest, newlyClaimed []string, enqueuedAt time.Time) run.QueueEntry {
	return run.QueueEntry{
		RunID:        req.traceID,
		SessionID:    req.sessionID,
		UserID:       req.userID,
		Content:      req.content,
		Model:        req.sel.Model,
		Selection:    selectionToQueue(req.sel),
		Attachments:  req.attachments,
		NewlyClaimed: newlyClaimed,
		RawToken:     req.rawToken,
		EnqueuedAt:   enqueuedAt,
	}
}

// selectionToQueue / selectionFromQueue convert between the handler's
// resolved modelSelection and the queue's wire-stable snapshot form.
func selectionToQueue(sel modelSelection) run.QueueSelection {
	return run.QueueSelection{
		Model:               sel.Model,
		Thinking:            sel.Thinking,
		Vision:              sel.Vision,
		Effort:              sel.Effort,
		MaxContextTokens:    sel.MaxContextTokens,
		MaxCompletionTokens: sel.MaxCompletionTokens,
		LengthExpandStep:    sel.LengthContinue.ExpandStep,
		LengthMaxRetries:    sel.LengthContinue.MaxRetries,
	}
}

func selectionFromQueue(q run.QueueSelection) modelSelection {
	return modelSelection{
		Model:               q.Model,
		Thinking:            q.Thinking,
		Vision:              q.Vision,
		Effort:              q.Effort,
		MaxContextTokens:    q.MaxContextTokens,
		MaxCompletionTokens: q.MaxCompletionTokens,
		LengthContinue: config.LengthContinueConfig{
			ExpandStep: q.LengthExpandStep,
			MaxRetries: q.LengthMaxRetries,
		},
	}
}
