package turn

// turn-start extraction (session-message-queue): Start is the shared
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
// memory recall (the preamble — deliberately INSIDE Start so a queued
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

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/memory"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/trace"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/skillmarket"
	"github.com/lush/blowball/pkg/stream"
)

// Start runs the full turn pipeline for a request whose session claim is
// held (or whose admission degraded to unguarded execution). It returns after
// the turn goroutine is spawned; the HTTP wrapper then subscribes SSE and
// parks on the result channel. All preamble work (history recovery, recall)
// happens here — after the claim, satisfying the send-time persistence
// invariants — so every entry path produces the identical turn.
func (e *Executor) Start(ctx context.Context, req *Request) (*Started, error) {
	// Source selection is FIRST and fail-closed: a scheduled turn must have
	// the scheduled persistence services, and no later code path may fall back
	// to the ordinary tables.
	srcExec, err := e.forSource(req.Source)
	if err != nil {
		return nil, err
	}
	e = srcExec

	sessionID, userID, tid := req.SessionID, req.UserID, req.RunID
	ctx = agent.WithUserID(ctx, userID)
	ctx = agent.WithSessionID(ctx, sessionID)
	ctx = agent.WithRuntimeStatusStore(ctx, e.runtimeStatus)

	sel := req.Selection
	turnModel := sel.Model
	turnLimit := sel.MaxContextTokens
	if turnLimit == 0 {
		// Safety net: a hand-built selection config without a window (tests)
		// falls back to the compaction service's own limit. Production
		// wiring derives both from the same mandatory catalog, so they agree.
		turnLimit = e.compSvc.FallbackMaxContext()
	}

	// ── preamble: history recovery, compaction stitching, memory recall ────
	prior, err := e.msgSvc.RecoverMessages(ctx, userID, sessionID)
	if err != nil {
		logger.FromContext(ctx).Warn("recover messages failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.Error(err))
		prior = nil
	}
	priorCalls, cerr := e.msgSvc.ListToolCalls(ctx, sessionID)
	if cerr != nil {
		logger.FromContext(ctx).Warn("recover tool calls failed; proceeding without",
			zap.String("op", "handler.send_message"),
			zap.Error(cerr))
		priorCalls = nil
	}

	agentMsgs, lastRow, err := MessagesToAgentMessagesIndexed(prior, priorCalls)
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
	if e.compSvc.Enabled() && req.ContextCompacted {
		if durable, durableCalls, derr := e.compSvc.RecoverDurable(ctx, sessionID); derr == nil {
			durableMsgs, durableLastRow, rerr := MessagesToAgentMessagesIndexed(durable, durableCalls)
			if rerr != nil {
				logger.FromContext(ctx).Warn("durable reconstruct failed; using full recovered history",
					zap.String("op", "handler.send_message"),
					zap.Error(rerr))
			} else {
				prior = durable
				agentMsgs = durableMsgs
				lastRow = durableLastRow
				if activeRecord := e.compSvc.LatestCompactionRecord(ctx, sessionID); activeRecord != nil {
					if stitched, attribution := stitchCompactedIndexed(durable, durableCalls, durableMsgs, durableLastRow, activeRecord); stitched != nil {
						agentMsgs = stitched
						lastRow = attribution
					}
				}
			}
		} else {
			logger.FromContext(ctx).Warn("durable recovery failed; using full recovered history",
				zap.String("op", "handler.send_message"),
				zap.Error(derr))
		}
	}
	agentMsgs = e.replaySessionRuntimeStatus(ctx, sessionID, agentMsgs, lastRow, prior)

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
	var recall *RecallInjection
	if e.memSvc.Enabled() {
		injected := InjectedRecallKeys(agentMsgs)
		if res, err := e.memSvc.Recall(ctx, userID, req.Content, injected); err != nil {
			logger.FromContext(ctx).Warn("memory recall failed; continuing without memory",
				zap.String("op", "handler.send_message"),
				zap.String("user_id", userID),
				zap.Error(err))
		} else if res.Frame != "" {
			inj := NewRecallInjection(sessionID, tid, req.Content, res)
			agentMsgs = append(agentMsgs, inj.AgentMessages()...)
			recall = &inj
		}
	}
	content := req.Content
	// Current-turn inline images (K=1 window): the LIVE user message carries
	// the image bytes read from disk; from the next turn on, reconstruction
	// renders this row as text placeholders instead. A missing file degrades
	// to the placeholder (ERROR log) — the turn must not die because a file
	// vanished underneath the registry.
	llmContent := content
	if req.LLMContextPrefix != "" {
		llmContent = req.LLMContextPrefix + "\n\n" + content
	}
	current := agent.Message{Role: "user", Content: llmContent}
	if len(req.Attachments) > 0 {
		current.Images, current.Content = e.loadCurrentTurnImages(ctx, userID, sessionID, llmContent, req.Attachments)
	}
	messages := append(agentMsgs, current)

	// Turn-scoped persistence bookkeeping shared by the send-time user-row
	// write, the mid-turn flush, and the turn-end save (context-compaction +
	// send-time-user-message-persistence): how many merged events have already
	// been persisted determines the suffix the turn-end save pushes, and
	// whether the user row was persisted at send time gates its inclusion in
	// every later batch — keeping both the MySQL rows (deterministic
	// client_msg_ids) and the Redis msgs:{sid} list free of duplicates.
	persist := PersistInfo{
		SessionID:       sessionID,
		UserID:          userID,
		TraceID:         tid,
		UserContent:     content,
		UserAttachments: req.Attachments,
		UserMsgTime:     req.UserMsgTime,
	}
	flushed := &FlushState{}

	// The synchronized event tap is UNCONDITIONAL (incremental-message-
	// persistence): the incremental persister below snapshots the collected
	// event stream through it every second, and the mid-turn compaction hook
	// (when compaction is configured) flushes through the same cursor state.
	// Both are per-turn values handed to the orchestrator alongside the hub.
	var hooks TurnHooks
	tap := NewTurnEventTap()
	if e.compSvc.Enabled() {
		inputBudget := configuredInputBudget(turnLimit, sel.MaxCompletionTokens)
		hooks = TurnHooks{
			Round: e.newBudgetedRoundHook(tap, persist, flushed, turnLimit, inputBudget, turnModel),
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
					return e.compactMidTurn(ctx, tap, persist, flushed, turnLimit, inputBudget, turnModel, estimate.Tokens, true, model.CompactionTriggerMidTurn)
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
	// A background fence is the last read-only checkpoint. It must pass before
	// title generation, durable writes, heartbeats, or any model call.
	if req.BeforeStart != nil {
		if err := req.BeforeStart(ctx); err != nil {
			return nil, err
		}
	}
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
	if e.titleSvc != nil {
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
			go e.titleSvc.GenerateTitle(ctx, sessionID, firstUserMsg, content)
		}
	}

	// ── run meta (queued), send-time persistence, heartbeat, running ───────
	//
	// Shutdown checkpoint (review fix, round 3, pump path only): the entry
	// was popped and its claim transferred, but if the process is shutting
	// down NOW the turn must not start — nothing durable has been written
	// yet (the preamble above is read-only), so the clean move is to return
	// the entry to the queue and let the post-restart reconciler continue
	// it. The HTTP path never aborts here: an in-flight request is covered
	// by srv.Shutdown waiting for the handler plus the registry's CancelAll.
	if req.FromQueue && e.newTurnsStopped() {
		return nil, ErrShutdownDuringStartup
	}

	// Run meta (ownership + status + the turn's resolved model) in Redis,
	// published in the QUEUED state before any durable write of this turn
	// (review fix, round 4): during the send-time persistence below the
	// status stays queued, and writeRunEventStream's dead-run ladder only
	// fires on running — so a slow write (dual-tier outage, pool contention)
	// needs no heartbeat at all; the already-tailing queue SSE simply keeps
	// waiting, exactly as it did while the message sat in the queue. A
	// pumped entry's meta was published atomically with its admission; a
	// FRESH run initializes it here; a meta that expired under a long wait
	// (the reconciler's keep-alive is best-effort) is re-initialized in FULL
	// from the request/entry snapshot — a status-only rebuild would leave
	// the cancel / events endpoints' ownership checks 404-ing (review fix,
	// round 1). Best-effort throughout: a failed write costs cancel/resume
	// for this run, never the turn itself.
	if _, ok, err := e.runs.Store.GetMeta(ctx, tid); err != nil || !ok {
		if err := e.runs.Store.InitMeta(ctx, tid, run.RunMeta{
			SessionID: sessionID,
			UserID:    userID,
			Status:    run.StatusQueued,
			Model:     turnModel,
			CreatedAt: req.MetaCreatedAt.Format(time.RFC3339),
		}); err != nil {
			logger.FromContext(ctx).Warn("run meta init failed",
				zap.String("op", "handler.send_message"),
				zap.String("run_id", tid),
				zap.Error(err))
		}
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
		placeholder, call := recall.PersistRows(persist.SessionID, persist.TraceID, persist.UserMsgTime)
		if err := e.sessSvc.SaveFinalMessages(ctx, userID, []model.Message{placeholder}, []model.ToolCall{call}); err != nil {
			logger.FromContext(ctx).Warn("send-time recall pair persist failed; continuing with in-memory injection only",
				zap.String("op", "handler.send_message"),
				zap.String("run_id", tid),
				zap.Error(err))
		}
	}
	if err := e.sessSvc.SaveMessagesBatch(ctx, userID, []model.Message{
		UserMessage(persist.SessionID, persist.TraceID, persist.UserContent, persist.UserMsgTime, persist.UserAttachments...),
	}); err != nil {
		logger.FromContext(ctx).Warn("send-time user message persist failed; deferring the user row to the turn-end batch",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	} else {
		flushed.MarkUserPersisted()
	}

	// ── heartbeat, then publish running ────────────────────────────────────
	//
	// The heartbeat comes AFTER the send-time persistence (review fix, round
	// 4): in the reverse order an observer could see Alive=true while the
	// user row was still in flight — a heartbeat proves the process is alive,
	// not that the row (and its attachment references) is durable, which is
	// what the queued-cancel reference guard needs. With this order two
	// invariants hold together:
	//
	//   running ⇒ alive — the status flip below happens only after the
	//   synchronous beat (round 1), and the startup keeper sustains the beat
	//   at the drainer's cadence until the drainer takes over (round 2), so
	//   a tailing subscriber never sees running without a live heartbeat;
	//
	//   alive ⇒ row write attempted — the beat follows the send-time write,
	//   so a running turn's row is either already readable through the
	//   shared persistence or its send-time write failed (the row then lands
	//   at turn end); the reference guard checks row observability directly
	//   rather than trusting the heartbeat for it.
	if err := e.runs.Store.Heartbeat(ctx, tid, sessionID); err != nil {
		logger.FromContext(ctx).Warn("pre-publish heartbeat failed; proceeding",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	}
	stopStartupHeartbeat := startPersistHeartbeat(ctx, e.runs.Store, tid, sessionID, run.HeartbeatEvery)
	defer stopStartupHeartbeat() // safety net; the explicit stop below is the normal exit

	// Publish running — both paths flip here (the meta was ensured in the
	// queued state above), AFTER the row write and the first heartbeat.
	if err := e.runs.Store.SetStatus(ctx, tid, run.StatusRunning); err != nil {
		logger.FromContext(ctx).Warn("run status flip to running failed",
			zap.String("op", "handler.send_message"),
			zap.String("run_id", tid),
			zap.Error(err))
	}

	// The turn context: detached, but carrying the same trace/session
	// attribution (raw capture, llm_raw_log) as the request context did — and
	// the login token, which flows from here into every tool execution ctx so
	// luban/executor market lookups survive the HTTP request's lifetime.
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turnCtx = skillmarket.WithToken(agent.WithUserID(agent.WithSessionID(trace.WithContext(turnCtx, tid), sessionID), agent.UserIDFromContext(ctx)), req.RawToken)
	turnCtx = agent.WithRuntimeStatusStore(turnCtx, e.runtimeStatus)
	// Sub-agent transcript persistence follows the SAME source as the turn.
	// A scheduled turn injects a scheduled-bound snapshot store; the
	// orchestrator's process-wide default is never consulted for it.
	if e.snapshots != nil {
		turnCtx = agent.WithSubAgentSnapshotStore(turnCtx, e.snapshots)
	}
	regRun := e.runs.Registry.Register(tid, turnCancel)

	workspaceRoot := filepath.Join(e.dataDir, userID, "workspace")
	skillsDir := filepath.Join(e.dataDir, userID, "skills")

	// Turn-artifacts capability: the turn-end finalization closure. The
	// adapter invokes it with this turn's top-level assistant text just
	// before the done event; linked paths are the artifact declaration.
	if e.artifacts != nil {
		svc := e.artifacts
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

	hub := e.newHub()

	// The drainer is the hub's single consumer: it appends every event to
	// run:{rid}:events (done included), heartbeats, and polls the
	// cross-process cancel flag. drained closes after the final drain, which
	// orders the terminal status write strictly after the last event.
	drainDone := run.StartDrainer(hub, e.runs.Store, e.runs.Registry, tid, sessionID, run.HeartbeatEvery)

	// The drainer's goroutine performs its own first beat immediately — the
	// startup keeper's job is done (the safety-net defer above is now a
	// no-op: stop is idempotent).
	stopStartupHeartbeat()

	resultCh := make(chan Result, 1)

	// saveCtx is a detached context that survives the HTTP request so the
	// turn-end persistence is not killed by a client disconnect. It carries
	// the turn's session_id + trace_id so its log sites (via logger.FromContext)
	// correlate with the turn (tool-call-log-correlation).
	saveCtx := agent.WithSessionID(trace.WithContext(context.Background(), tid), sessionID)

	// Mid-turn incremental persister (incremental-message-persistence): the
	// closed merged-event prefix lands every second while the turn runs; the
	// turn goroutine stops it (draining any in-flight batch) before the
	// terminal batch computes its suffix.
	stopIncremental := e.startIncrementalPersister(saveCtx, tap, persist, flushed)

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
	persistTurnEvents := func(events []stream.StreamEvent, usage map[string]any, interrupted bool) (durable bool) {
		durable = true
		defer func() {
			if r := recover(); r != nil {
				durable = false
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
		// The submitted content is the persisted pair (persist.UserContent
		// + top-level assistant tokens); recall tool-pair rows are never
		// captured (capture sources only the user/assistant pair).
		if e.memSvc.Enabled() {
			tc := memory.TurnCapture{
				UserID:        userID,
				SessionID:     sessionID,
				UserContent:   persist.UserContent,
				AssistantText: topLevelAssistantText(merged),
				UserAt:        persist.UserMsgTime,
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
				if err := e.memSvc.CaptureTurn(saveCtx, tc); err != nil {
					logger.FromContext(saveCtx).Warn("memory capture failed; turn unaffected",
						zap.String("op", "handler.send_message"),
						zap.String("user_id", tc.UserID),
						zap.Error(err))
				}
			}(tc)
		}

		batch, consumed := persist.BuildRoundFinalBatch(merged, flushed.Count(), flushed.UserPersisted(), now,
			RoundRenderOptions{Terminal: true, Interrupted: interrupted})

		if len(batch.Msgs) > 0 || len(batch.Calls) > 0 {
			// Bounded retry (harden-turn-persistence D4): the batch carries
			// deterministic client_msg_ids, so redelivery collapses to one
			// row per message — retrying the whole SaveMessagesBatch is
			// safe. A dual-tier outage delays the terminal batch (and with
			// it the claim release) by at most the budget; after the budget
			// it is an ERROR, not a silent loss.
			var lastErr error
			deadline := time.Now().Add(turnPersistBudget)
			for attempt := 1; attempt <= turnPersistAttempts; attempt++ {
				if err := e.sessSvc.SaveFinalMessages(saveCtx, userID, batch.Msgs, batch.Calls); err != nil {
					lastErr = err
					logger.FromContext(saveCtx).Warn("turn-end message persist attempt failed",
						zap.String("op", "handler.send_message"),
						zap.Int("attempt", attempt),
						zap.Int("rows", len(batch.Msgs)),
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
				durable = false
				logger.FromContext(saveCtx).Error("turn-end message persist budget exhausted; batch not durable",
					zap.String("op", "handler.send_message"),
					zap.String("session_id", sessionID),
					zap.String("run_id", tid),
					zap.Int("rows", len(batch.Msgs)),
					zap.Error(lastErr))
			}
			flushed.Mark(consumed)
		}

		// Persist per-agent token cost into turn_usage AFTER the message
		// batch. Failure is logged only — never roll back messages.
		if tu, ok := BuildTurnUsage(sessionID, tid, userID, turnModel, usage); ok {
			if err := e.sessSvc.SaveTurnUsage(saveCtx, tu); err != nil {
				logger.FromContext(saveCtx).Warn("save turn_usage failed; messages persisted",
					zap.String("op", "handler.send_message"),
					zap.Error(err))
			}
		}
		return durable
	}

	go func() {
		// Registry teardown: Unregister last so Finish (which unblocks
		// WaitAll) happens while the entry still resolves.
		defer e.runs.Registry.Unregister(tid)
		defer regRun.Finish()
		events, usage, err := e.orch.Handle(turnCtx, workspaceRoot, skillsDir, userID, messages, hub, hooks, sel.Override())
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
		stopHeartbeat := startPersistHeartbeat(saveCtx, e.runs.Store, tid, sessionID, run.HeartbeatEvery)

		// Stop the incremental persister FIRST (blocking until any in-flight
		// batch resolves) so the terminal batch below is exclusive and its
		// suffix starts at the final cursor value.
		stopIncremental()

		// Terminal persistence BEFORE Finalize (harden-turn-persistence D2):
		// the session claim is only released after this turn's message batch
		// is durable, making generating:false a reliable "history complete"
		// signal for clients — and, for the queue, the guarantee the pumped
		// next turn's history recovery relies on.
		durable := persistTurnEvents(events, usage, err != nil)

		// (Drain-before-release D5 is retired with the ingest queue: the
		// terminal batch above is a synchronous transactional MySQL write —
		// history is complete the moment it returns.)

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
		if req.OnTerminal != nil {
			req.OnTerminal(saveCtx, TerminalResult{
				RunID: tid, SessionID: sessionID, UserID: userID, Status: turnStatus,
				MessagesDurable: durable, Events: events, Usage: usage, Err: err,
			})
		}
		stopHeartbeat()
		finalizeCtx := agent.WithSessionID(trace.WithContext(context.Background(), tid), sessionID)
		e.runs.Finalize(finalizeCtx, tid, sessionID, turnStatus)
		turnCancel()

		resultCh <- Result{Events: events, Usage: usage, Err: err}

		// Queue pump tail (session-message-queue): after the terminal
		// bookkeeping released the claim, try to continue the session's
		// queued head as the next turn. Skipped when the queue is disabled
		// or the process is shutting down (entries stay enqueued for the
		// post-restart reconciler).
		if e.queuePump != nil {
			e.queuePump(sessionID)
		}
	}()

	return &Started{ResultCh: resultCh, TurnDone: regRun.Done()}, nil
}
