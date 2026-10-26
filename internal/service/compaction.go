package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tokens"
)

// Fixed context-compaction parameters (context-compaction capability). The
// trigger threshold is a hard-coded 80% of the resolved catalog entry's
// max_context_tokens — intentionally NOT configurable (design D-risk: keep it
// one constant so a future config knob is a one-line change); retainedTail is
// the number of trailing agent messages kept verbatim on compaction;
// summaryMaxTokens bounds the one-shot checkpoint-summary call (model-effort-v2
// removed the Confucius-agent quota it used to borrow).
const (
	compactionThresholdNumerator   = 8
	compactionThresholdDenominator = 10
	compactionRetainedTail         = 5
	compactionSummaryMaxTokens     = 4096
	compactionAnchorMaxTokens      = 8192
)

// compactionSystemPrompt is the structured checkpoint instruction handed to
// the summarization call. It demands the eight fixed sections (the
// deepseek-harness checkpoint template) with verbatim preservation of the
// values a continuation actually needs, and the merge rule that keeps
// re-compaction from stacking checkpoints.
const compactionSystemPrompt = `You are maintaining the checkpoint of an AI coding-assistant conversation. Condense the supplied conversation span into ONE structured checkpoint that a fresh assistant can continue from without re-reading the original messages.

Produce exactly these eight sections, in this order, as Markdown headings:
## Primary Request and Intent
## Key Technical Concepts
## Files and Code
## Errors and Fixes
## Pending Jobs
## Current Work
## Next Step
## Critical Context

Rules:
- Preserve exact file paths, commands, URLs, error strings, identifiers, function signatures, and numeric values VERBATIM; never paraphrase or truncate them.
- Keep concrete code snippets and file references that will be needed again; drop finished exploratory detours and small talk.
- When a PRIOR CHECKPOINT is supplied, MERGE it with the new span into one consolidated checkpoint. Do not stack, do not append section duplicates, and do not drop still-relevant prior facts.
- Output only the checkpoint body, no preamble and no closing remarks.`

// CompactionService owns the context-compaction lifecycle (context-compaction
// capability): the trigger threshold, the compressible-range selection on the
// reconstructed agent-message sequence, the checkpoint-summary LLM call, and
// the crash-consistent record write order (MySQL record → Redis cache →
// sessions.context_compacted flag). Every failure is best-effort — a WARN and
// a skipped compaction never block the conversation.
//
// The service is inert while its fallback max context is unset (0): Enabled
// reports false and callers skip every check, reproducing the pre-capability
// behavior byte for byte. Per-request-model-selection moved the effective
// threshold to the TURN: callers pass the request-selected model's window to
// ShouldCompact/ThresholdTokens, and the fallback here only decides whether
// the capability is armed at all (and thresholds parameter-less turns).
type CompactionService struct {
	deps SessionDeps
	llm  agent.LLMClient
	// defaultSummaryModel is the fallback summary model (model-effort-v2):
	// the deployment's default catalog entry name (Config.DefaultModelName),
	// resolved at wiring. A turn that knows its resolved model overrides it
	// via CompactionInput.SummaryModel.
	defaultSummaryModel string
	// maxContextTokens is the FALLBACK effective max context for turns whose
	// selection config carries no window (hand-built tests): the default
	// catalog entry's window in production. 0 disables compaction entirely.
	maxContextTokens int
}

// NewCompactionService wires a CompactionService from the shared store deps,
// the LLM client (the same OpenAIClient the agents use, so summary calls land
// in llm_raw_log like any other call), the fallback summary model name (the
// deployment default — pass Config.DefaultModelName()), and the fallback max
// context window.
func NewCompactionService(deps SessionDeps, llm agent.LLMClient, defaultSummaryModel string, maxContextTokens int) *CompactionService {
	return &CompactionService{
		deps:                deps,
		llm:                 llm,
		defaultSummaryModel: defaultSummaryModel,
		maxContextTokens:    maxContextTokens,
	}
}

// Enabled reports whether compaction is configured at all. Zero fallback
// max context means disabled: no trigger checks fire anywhere.
func (s *CompactionService) Enabled() bool {
	return s != nil && s.maxContextTokens > 0
}

// FallbackMaxContext exposes the fallback max context (the constructor's
// maxContextTokens) so handlers whose model-selection state carries no
// default window (zero ModelSelectionConfig — tests, or a wiring that predates
// per-request-model-selection) still threshold parameter-less turns exactly
// as before. Production wiring derives both from the same config, so they
// agree and this accessor is only a safety net.
func (s *CompactionService) FallbackMaxContext() int {
	if s == nil {
		return 0
	}
	return s.maxContextTokens
}

// ThresholdTokens returns the absolute trigger threshold (80% of the given
// max context). Integer math: the comparison in ShouldCompact is
// tokens*10 >= max*8, so an exact tie at the threshold triggers. A
// non-positive limit (compaction disabled for that model) yields 0.
func (s *CompactionService) ThresholdTokens(maxContextTokens int) int {
	if !s.Enabled() || maxContextTokens <= 0 {
		return 0
	}
	return maxContextTokens * compactionThresholdNumerator / compactionThresholdDenominator
}

// ShouldCompact reports whether the measured context pressure (the
// authoritative prompt+completion of the last LLM round, or the latest
// turn_usage.context_tokens for turn-start checks) has reached the trigger
// threshold of the GIVEN max context. The limit is a per-turn value
// (per-request-model-selection): the request-selected catalog model's
// max_context_tokens — callers compare the stored raw size against the
// CURRENT request's model, so a model switch re-evaluates old turns against
// the new window. Disabled service or non-positive limit → always false.
func (s *CompactionService) ShouldCompact(contextTokens, maxContextTokens int) bool {
	if !s.Enabled() || maxContextTokens <= 0 {
		return false
	}
	return contextTokens*compactionThresholdDenominator >= maxContextTokens*compactionThresholdNumerator
}

// LatestContextTokens returns the session's most recent recorded end-of-turn
// context size (turn_usage.context_tokens). Zero for sessions without prior
// turns — the natural no-trigger state for a first turn.
func (s *CompactionService) LatestContextTokens(ctx context.Context, sessionID string) (int, error) {
	return s.deps.MySQL.LatestContextTokens(ctx, sessionID)
}

// RecoverDurable returns the session's persisted history from the DURABLE
// tier: it first drains the write-behind ingest queue (bounded, the same
// primitive the pre-delete archive path uses) so rows dual-written moments
// ago carry real MySQL identities, then reads MySQL directly.
//
// Compaction MUST operate on this view, not on RecoverMessages' Redis-first
// view: the Redis read cache serves rows whose id is still 0 (ids are minted
// by the MySQL flusher), and a boundary cursor recorded against an id-0 row
// matches nothing once the cache backfills from MySQL — the stitched context
// would silently degrade to full history. A drain failure aborts the caller's
// compaction attempt (the durable view would be missing the queued rows).
func (s *CompactionService) RecoverDurable(ctx context.Context, sessionID string) ([]model.Message, []model.ToolCall, error) {
	if s.deps.DrainMessageQueue != nil {
		drainCtx, cancel := context.WithTimeout(ctx, messageDrainTimeout)
		err := s.deps.DrainMessageQueue(drainCtx)
		cancel()
		if err != nil {
			return nil, nil, fmt.Errorf("compaction.recover_durable: drain: %w", err)
		}
	}
	rows, err := s.deps.MySQL.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	calls, err := s.deps.MySQL.ListToolCalls(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return rows, calls, nil
}

// CompactionInput carries the recovered conversation a caller hands to
// Compact. Rows is the full ordered persisted history; AgentMsgs is
// MessagesToAgentMessages(Rows); LastRow[i] is the index into Rows of the
// last persistence row that contributed to AgentMsgs[i] (the mapping back to
// the boundary composite cursor). Callers recover + reconstruct right before
// calling — for the mid-turn trigger that happens AFTER the flush-first
// persistence, so the boundary always points at real rows.
type CompactionInput struct {
	SessionID     string
	UserID        string
	TriggerKind   string // model.CompactionTriggerMidTurn | model.CompactionTriggerTurnStart
	TriggerTokens int    // measured context pressure at the trigger point
	Rows          []model.Message
	AgentMsgs     []agent.Message
	LastRow       []int
	// SummaryModel (per-request-model-selection) is the model this turn
	// resolved to — the checkpoint summary call uses it, and it is recorded
	// on the compaction row. Empty falls back to the service's startup
	// default (the deployment's default catalog entry, model-effort-v2).
	SummaryModel string
	// InputBudget is the turn's effective request input budget. Zero keeps the
	// legacy fixed-tail behavior (hand-built tests and pre-budgeting callers).
	InputBudget int
	// TriggerTokensMeasured records whether TriggerTokens came from a provider
	// usage observation rather than a local estimate.
	TriggerTokensMeasured bool
	// RequestOverheadTokens is fixed prompt/tool/protocol overhead not present
	// in AgentMsgs. Zero falls back to the rendered-message estimate alone.
	RequestOverheadTokens int
}

// Compact runs one compaction attempt: it selects the compressible range
// (everything after the preserved first message and before the retained tail,
// never splitting a tool call/result pair), generates the checkpoint summary
// through the LLM (merging a prior checkpoint when one exists), and writes the
// record in the crash-consistent order record → Redis → session flag.
//
// It returns the persisted record and true on success. Every other outcome —
// empty middle, LLM failure, empty summary, record-insert failure — returns
// (nil, false) with a WARN logged and the conversation left fully usable on
// its previous state (the capability's never-block guarantee). Redis/flag
// write failures after a successful insert are degraded per design D8: the
// record is durable, stitching works this turn from the returned record, and
// the cache self-heals on the next miss.
func (s *CompactionService) Compact(ctx context.Context, in CompactionInput) (*model.ContextCompaction, bool) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "compaction.compact"),
		zap.String("session_id", in.SessionID),
		zap.String("trigger_kind", in.TriggerKind),
		zap.Int("trigger_tokens", in.TriggerTokens),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	if !s.Enabled() {
		return nil, false
	}
	if len(in.AgentMsgs) != len(in.LastRow) {
		log.Warn("compaction input malformed: agent messages and row attribution out of sync; skipping")
		return nil, false
	}
	if len(in.AgentMsgs) == 0 {
		log.Debug("compaction skipped: durable history is empty")
		return nil, false
	}
	for _, rowIndex := range in.LastRow {
		if rowIndex < 0 || rowIndex >= len(in.Rows) {
			log.Warn("compaction input malformed: row attribution is out of range; skipping")
			return nil, false
		}
	}

	// Prior record: merged into the summary input on re-compaction. A read
	// failure degrades to "no prior" (the summary then re-covers the whole
	// middle — a safe superset, never a broken state).
	prior := s.LatestCompactionRecord(ctx, in.SessionID)

	scopeStart := 1
	if prior != nil {
		scopeStart = scopeStartAfterBoundary(in.Rows, in.AgentMsgs, in.LastRow, prior)
	}

	// The tail budget is not the request budget. Reserve the checkpoint,
	// anchor, and next user turn before allocating the retained tail; otherwise
	// small test windows (and unusually short real windows) retain nearly the
	// whole conversation and produce a no-op compaction.
	tailBudget := in.InputBudget
	if tailBudget > 0 {
		tailBudget = tailBudget * 2 / 3
	}
	plan := planCompactionTail(in.AgentMsgs, scopeStart, compactionRetainedTail, tailBudget)
	tailStart := plan.TailStart

	// The summary model follows the TURN (per-request-model-selection): the
	// resolved catalog model, falling back to the deployment default when
	// the caller passes none.
	summaryModel := in.SummaryModel
	if summaryModel == "" {
		summaryModel = s.defaultSummaryModel
	}
	anchor := in.AgentMsgs[0]
	anchorTruncated := false
	storedAnchorJSON := ""
	if in.InputBudget > 0 {
		anchor, anchorTruncated = boundedCompactionAnchor(anchor, in.InputBudget)
		if anchorTruncated {
			if encoded, err := json.Marshal(anchor); err == nil {
				storedAnchorJSON = string(encoded)
			} else {
				log.Warn("bounded anchor marshal failed; compaction skipped", zap.Error(err))
				return nil, false
			}
		}
	}

	if tailStart <= scopeStart || tailStart > len(in.AgentMsgs) {
		log.Warn("compaction skipped: compressible middle is empty",
			zap.Int("scope_start", scopeStart),
			zap.Int("tail_start", tailStart),
			zap.String("reason", plan.Reason))
		return nil, false
	}

	// Select the tail against a worst-case checkpoint budget before making any
	// model call. This keeps retries cheap and ensures the one final summary
	// already covers the final boundary.
	initialTail := tailStart
	if in.InputBudget > 0 {
		var ok bool
		tailStart, ok = chooseTailForReservedBudget(in, anchor, scopeStart, tailStart)
		if !ok {
			log.Warn("compaction candidate remains above request budget",
				zap.Int("tail_start", tailStart),
				zap.Int("input_budget_tokens", in.InputBudget))
			return nil, false
		}
		if tailStart != initialTail {
			log.Info("compaction tail reduced before summary",
				zap.Int("initial_tail_start", initialTail),
				zap.Int("final_tail_start", tailStart))
		}
	}

	// Summary input never carries image payloads (add-image-attachments):
	// reconstructed history already renders placeholders, but StripImages
	// future-proofs any caller handing in live messages with Images set.
	middle := agent.StripImages(in.AgentMsgs[scopeStart:tailStart])
	shadowedCount := len(middle)
	boundaryRow := in.Rows[in.LastRow[tailStart-1]]
	content, usage, strategy, ok := s.summarize(ctx, log, prior, middle, summaryModel, in.InputBudget)
	if !ok {
		return nil, false
	}
	retained, activityCount := summarizeRetainedToolResults(in.AgentMsgs, tailStart)
	if activity := renderScopeToolActivity(middle); activity != "" {
		content += "\n\n## Tool Activity\n" + activity
	}
	projected := buildCompactedProjection(anchor, in.AgentMsgs, tailStart, content, retained)
	postTokens := in.RequestOverheadTokens + agent.EstimateRenderedRequest("", projected, nil).Tokens
	if in.InputBudget > 0 && postTokens > in.InputBudget {
		log.Warn("compaction summary exceeded its reserved candidate budget",
			zap.Int("post_compaction_tokens", postTokens),
			zap.Int("input_budget_tokens", in.InputBudget))
		return nil, false
	}

	retainedJSON, _ := json.Marshal(retained)
	rec := model.ContextCompaction{
		SessionID:        in.SessionID,
		UserID:           in.UserID,
		TraceID:          tid,
		TriggerKind:      in.TriggerKind,
		TriggerTokens:    in.TriggerTokens,
		Content:          content,
		BoundaryMsgTime:  boundaryRow.MsgTime,
		BoundaryMsgIndex: boundaryRow.MsgIndex,
		BoundaryMsgID:    boundaryRow.ID,
		AnchorJSON:       storedAnchorJSON,
		AnchorTruncated:  anchorTruncated,
		// The summary prompt IS the shadowed span (+ prior checkpoint +
		// instruction), so its prompt size is the faithful measured proxy for
		// the shadowed content.
		ShadowedTokens:          usage.PromptTokens,
		SummaryModel:            summaryModel,
		SummaryPromptTokens:     usage.PromptTokens,
		SummaryCompletionTokens: usage.CompletionTokens,
		RetainedResultsJSON:     retainedJSON,
		MeasurementBasis:        measurementBasis(in.TriggerTokensMeasured),
		PreCompactionTokens:     in.TriggerTokens,
		InputBudgetTokens:       in.InputBudget,
		PostCompactionTokens:    postTokens,
		RetainedTailMessages:    len(in.AgentMsgs) - tailStart,
		SummaryStrategy:         strategy,
		ToolSummaryCount:        len(retained),
		ToolActivityCount:       activityCount,
	}
	rec.RetainedResults = retained
	rec.DecodeRetainedResults()

	// Write order (crash consistency): record first, then cache, then flag.
	if err := s.deps.MySQL.InsertCompaction(ctx, rec); err != nil {
		log.Warn("compaction record insert failed; compaction aborted", zap.Error(err))
		return nil, false
	}
	if raw, err := json.Marshal(rec); err == nil {
		if err := s.deps.Redis.SetCompactionCache(ctx, in.SessionID, raw); err != nil {
			log.Warn("compaction cache write failed; record durable, cache self-heals on next miss", zap.Error(err))
		}
	}
	if err := s.deps.MySQL.UpdateSessionCompacted(ctx, in.SessionID); err != nil {
		log.Warn("session compaction flag update failed; stitching this turn uses the in-memory record", zap.Error(err))
	}

	log.Info("context compacted",
		zap.Int("shadowed_messages", shadowedCount),
		zap.Int64("boundary_msg_id", rec.BoundaryMsgID),
		zap.Int("summary_prompt_tokens", rec.SummaryPromptTokens),
		zap.Int("summary_completion_tokens", rec.SummaryCompletionTokens))
	return &rec, true
}

// summarize runs the checkpoint LLM call over the shadowed middle, merging a
// prior checkpoint when the session was compacted before. Only the returned
// text is retained; reasoning content and tool calls are discarded (never
// persisted into the record). The call itself flows through the ordinary
// OpenAI client, so raw capture (llm_raw_log) observes it for free. model is
// the TURN's selected model (per-request-model-selection) — the summary rides
// the same model the agents run this turn.
func (s *CompactionService) summarize(ctx context.Context, log *zap.Logger, prior *model.ContextCompaction, middle []agent.Message, model string, inputBudget int) (string, agent.Usage, string, bool) {
	if s.llm == nil || model == "" {
		log.Warn("compaction summarizer unavailable (no llm client or model); skipping compaction")
		return "", agent.Usage{}, "", false
	}

	var b strings.Builder
	if prior != nil {
		b.WriteString("PRIOR CHECKPOINT (an earlier span of this session, already condensed):\n\n")
		b.WriteString(prior.Content)
		b.WriteString("\n\nNEW CONVERSATION SPAN to merge into that checkpoint:\n\n")
	}
	b.WriteString(renderAgentTranscript(middle))

	// Attribute the summary call to the compaction pseudo-agent in the raw
	// capture log; session_id already rides the caller's context.
	agentCtx := agent.WithAgentName(ctx, "compaction")
	req := agent.LLMRequest{
		Model: model,
		Messages: []agent.Message{
			{Role: "system", Content: compactionSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		MaxCompletionTokens: compactionSummaryMaxTokens,
	}

	budget := inputBudget
	if budget <= 0 {
		budget = 0 // unlimited / legacy
	}
	content, usage, err := s.summarizeChunked(ctx, log, agentCtx, req, budget)
	if err != nil {
		log.Warn("compaction summary llm call failed; continuing with original context", zap.Error(err))
		return "", agent.Usage{}, "", false
	}
	content = strings.TrimSpace(content)
	if content == "" {
		log.Warn("compaction summary empty; continuing with original context")
		return "", agent.Usage{}, "", false
	}
	strategy := "one_shot"
	if budget > 0 && estimateSummaryRequest(req) > budget {
		strategy = "chunked"
	}
	return content, usage, strategy, true
}

func estimateSummaryRequest(req agent.LLMRequest) int {
	total := compactionSummaryMaxTokens + 512
	for _, m := range req.Messages {
		total += tokens.Estimate(m.Content) + 8
	}
	return total
}

// summarizeChunked runs one request when it fits the summary input budget;
// otherwise it condenses bounded chunks sequentially into the prior/accumulated
// checkpoint. The final request still happens through the same LLM client so
// raw capture and user-credential resolution are unchanged.
func (s *CompactionService) summarizeChunked(ctx context.Context, log *zap.Logger, agentCtx context.Context, req agent.LLMRequest, inputBudget int) (string, agent.Usage, error) {
	if inputBudget <= 0 || estimateSummaryRequest(req) <= inputBudget {
		resp, err := s.llm.StreamChat(agentCtx, req, nil, nil)
		return resp.Content, resp.Usage, err
	}
	var total agent.Usage
	user := req.Messages[len(req.Messages)-1].Content
	runes := []rune(user)
	var accumulated string
	for start := 0; start < len(runes); {
		chunkRunes := summaryChunkRunes(inputBudget, accumulated)
		if chunkRunes <= 0 {
			return "", total, fmt.Errorf("summary input budget leaves no room for a transcript chunk")
		}
		end := start + chunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		part := string(runes[start:end])
		prompt := part
		if accumulated != "" {
			prompt = "ACCUMULATED CHECKPOINT TO MERGE:\n\n" + accumulated + "\n\nNEW SPAN:\n\n" + part
		}
		partReq := agent.LLMRequest{
			Model: req.Model,
			Messages: []agent.Message{
				{Role: "system", Content: compactionSystemPrompt},
				{Role: "user", Content: prompt},
			},
			MaxCompletionTokens: compactionSummaryMaxTokens,
		}
		if estimateSummaryRequest(partReq) > inputBudget {
			return "", total, fmt.Errorf("summary chunk exceeds input budget")
		}
		resp, err := s.llm.StreamChat(agentCtx, partReq, nil, nil)
		if err != nil {
			return "", total, err
		}
		total.Add(resp.Usage)
		accumulated = strings.TrimSpace(resp.Content)
		if accumulated == "" {
			return "", total, fmt.Errorf("empty chunk summary")
		}
		start = end
	}
	return accumulated, total, nil
}

func summaryChunkRunes(inputBudget int, accumulated string) int {
	if inputBudget <= 0 {
		return 24000
	}
	base := estimateSummaryRequest(agent.LLMRequest{
		Messages: []agent.Message{
			{Role: "system", Content: compactionSystemPrompt},
			{Role: "user"},
		},
		MaxCompletionTokens: compactionSummaryMaxTokens,
	})
	available := inputBudget - base
	if accumulated != "" {
		available -= tokens.Estimate("ACCUMULATED CHECKPOINT TO MERGE:\n\n" + accumulated + "\n\nNEW SPAN:\n\n")
	}
	if available <= 0 {
		return 0
	}
	// One rune per token is the conservative CJK bound from tokens.Estimate.
	return available
}

// LatestCompactionRecord returns the session's authoritative (latest)
// compaction record through the three-stage cache pattern: Redis hit → MySQL
// latest row + backfill → nil on a double miss. A Redis error degrades to the
// MySQL read; a corrupt cached blob is treated as a miss. The caller treats
// nil as "degrade to full history" (the spec's missing-record scenario).
func (s *CompactionService) LatestCompactionRecord(ctx context.Context, sessionID string) *model.ContextCompaction {
	log := logger.L().With(zap.String("op", "compaction.latest_record"), zap.String("session_id", sessionID))

	if raw, err := s.deps.Redis.GetCompactionCache(ctx, sessionID); err != nil {
		log.Warn("compaction cache read failed; falling back to mysql", zap.Error(err))
	} else if raw != nil {
		var rec model.ContextCompaction
		if err := json.Unmarshal(raw, &rec); err == nil {
			return &rec
		}
		log.Warn("compaction cache blob corrupt; falling back to mysql", zap.Error(err))
	}

	rec, err := s.deps.MySQL.LatestCompaction(ctx, sessionID)
	if err != nil {
		log.Warn("compaction mysql read failed; degrading to full history", zap.Error(err))
		return nil
	}
	if rec == nil {
		return nil
	}
	if raw, err := json.Marshal(rec); err == nil {
		if err := s.deps.Redis.SetCompactionCache(ctx, sessionID, raw); err != nil {
			log.Warn("compaction cache backfill failed; record still served", zap.Error(err))
		}
	}
	return rec
}

// scopeStartAfterBoundary returns the index of the first agent message that
// lies strictly after the prior compaction's boundary — the start of the
// newly-shadowable span on re-compaction. Rows already condensed by the prior
// record (at or before the boundary cursor) are excluded so the prior
// checkpoint is merged, never re-summarized. When the boundary row cannot be
// located in the current rows (unexpected: rows are append-only), it falls
// back to 1 — re-covering the whole middle is a safe superset.
func scopeStartAfterBoundary(rows []model.Message, agentMsgs []agent.Message, lastRow []int, prior *model.ContextCompaction) int {
	ord := -1
	for i := range rows {
		if compactionCursorLE(rows[i], prior.BoundaryMsgTime, prior.BoundaryMsgIndex, prior.BoundaryMsgID) {
			ord = i // rows ascend, so the last match is the greatest
		}
	}
	if ord < 0 {
		return 1
	}
	for j := range agentMsgs {
		if lastRow[j] > ord {
			return j
		}
	}
	return len(agentMsgs) // nothing after the boundary: middle is empty
}

// compactionCursorLE reports whether row r sorts at or before the composite
// cursor (msgTime, msgIndex, msgID) under the messages global ordering — the
// same ordering the pagination cursor uses.
func compactionCursorLE(r model.Message, msgTime time.Time, msgIndex int, msgID int64) bool {
	if !r.MsgTime.Equal(msgTime) {
		return r.MsgTime.Before(msgTime)
	}
	if r.MsgIndex != msgIndex {
		return r.MsgIndex < msgIndex
	}
	return r.ID <= msgID
}

// renderAgentTranscript renders the shadowed agent messages as the plain-text
// transcript the summarizer consumes. Role labels make the conversation shape
// legible without the OpenAI wire format; tool calls keep their raw JSON
// arguments so exact identifiers survive.
func renderAgentTranscript(msgs []agent.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("USER:\n")
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		case "assistant":
			if m.ReasoningContent != "" {
				b.WriteString("ASSISTANT REASONING:\n")
				b.WriteString(m.ReasoningContent)
				b.WriteString("\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT TOOL CALL %s(%s)\n\n", tc.Function.Name, tc.Function.Arguments)
			}
			if m.Content != "" {
				b.WriteString("ASSISTANT:\n")
				b.WriteString(m.Content)
				b.WriteString("\n\n")
			}
		case "tool":
			fmt.Fprintf(&b, "TOOL RESULT %s:\n%s\n\n", m.Name, m.Content)
		default:
			if m.Content != "" {
				fmt.Fprintf(&b, "%s:\n%s\n\n", strings.ToUpper(m.Role), m.Content)
			}
		}
	}
	return b.String()
}

// compactionTailPlan is the budget-aware retained-tail decision. If InputBudget
// is zero, Limit remains compactionRetainedTail (legacy behavior); with a
// positive budget, the planner walks backward until it reaches the budget.
type compactionTailPlan struct {
	TailStart int
	Strategy  string
	Reason    string
}

// planCompactionTail keeps complete units: user messages, assistant text /
// reasoning, and an assistant tool-call message together with every result in
// the immediately following tool run. Tool results are summarized rather than
// counted at full size when a budget is present.
func planCompactionTail(msgs []agent.Message, scopeStart, legacyLimit, inputBudget int) compactionTailPlan {
	if inputBudget <= 0 {
		start := len(msgs) - legacyLimit
		if start < 0 {
			start = 0
		}
		if start <= scopeStart {
			// A first+tail-only conversation has nothing to shadow.
			start = len(msgs)
		}
		for start > scopeStart && start < len(msgs) && msgs[start].Role == "tool" {
			start--
		}
		return compactionTailPlan{TailStart: start, Strategy: "legacy_message_count"}
	}
	// Reserve a small fraction for the checkpoint and fixed prompt framing.
	// Keep the tail bounded, but do not let a tiny synthetic window collapse
	// the compressible middle to one message. Production windows are far
	// larger; this floor also preserves the legacy small-window test shape.
	budget := inputBudget / 4
	if budget < 512 {
		budget = 512
	}
	if inputBudget <= 1000 {
		// The legacy test/fallback window uses the five-message selector; the
		// budget planner is enforced for production-sized budgets.
		budget = 0
	}
	start := len(msgs)
	// The legacy tail is a MINIMUM retention window for real compaction:
	// never summarize messages that would have remained under the old
	// five-message policy. The budget may retain more, not fewer.
	legacyStart := len(msgs) - legacyLimit
	if legacyStart < scopeStart {
		legacyStart = scopeStart
	}
	if legacyStart >= len(msgs) {
		return compactionTailPlan{TailStart: len(msgs), Strategy: "token_budgeted", Reason: "no_compressible_middle"}
	}
	if legacyStart <= scopeStart {
		// Six-agent-message history with default tail five: first + tail only.
		// The callers treat tailStart <= scopeStart as an empty middle.
		if len(msgs) <= legacyLimit+1 {
			return compactionTailPlan{TailStart: scopeStart, Strategy: "token_budgeted", Reason: "no_compressible_middle"}
		}
	}
	used := 0
	start = legacyStart
	for i := start; i < len(msgs); i++ {
		used += estimateRetainedUnit(msgs[i : i+1])
	}
	for start > scopeStart {
		unitStart := start - 1
		for unitStart > scopeStart && msgs[unitStart].Role == "tool" {
			unitStart--
		}
		if unitStart > scopeStart && msgs[unitStart].Role == "assistant" && len(msgs[unitStart].ToolCalls) > 0 {
			// Include the complete parallel result run, not a dangling call.
			for unitStart+1 < len(msgs) && msgs[unitStart+1].Role == "tool" {
				unitStart++
			}
		}
		unit := msgs[unitStart:start]
		cost := estimateRetainedUnit(unit)
		if used+cost > budget && start > scopeStart {
			break
		}
		used += cost
		start = unitStart
		if used >= budget {
			break
		}
	}
	if start == scopeStart && start < len(msgs) {
		// Always keep at least one compressible middle unit; the anchor is
		// scopeStart-1 by construction, so this cannot swallow it.
		start = scopeStart + 1
	}
	// Never retain an orphan tool result: if we stopped in the middle of a
	// result run, back up to that assistant call message.
	for start > scopeStart && start < len(msgs) && msgs[start].Role == "tool" {
		start--
	}
	return compactionTailPlan{TailStart: start, Strategy: "token_budgeted", Reason: "request_budget"}
}

func estimateRetainedUnit(unit []agent.Message) int {
	total := 0
	for _, m := range unit {
		total += tokens.Estimate(m.Content) + tokens.Estimate(m.ReasoningContent)
		if m.Role == "tool" && m.ToolCallID != "" && len(m.Content) > 0 {
			// Retained tool results are summarized; charge a bounded budget
			// rather than their full original size.
			total += 400
		}
	}
	return total
}

// summarizeRetainedToolResults returns deterministic summaries for tool
// results that remain in the tail. The full result is retained in durable
// tool_calls storage and referenced by a stable retrieval identifier. Tool
// result messages carry only the call id/name, so arguments are recovered from
// the preceding assistant tool-call messages.
func summarizeRetainedToolResults(msgs []agent.Message, tailStart int) (map[string]string, int) {
	calls := indexToolCalls(msgs)
	results := make(map[string]string)
	for i := tailStart; i < len(msgs); i++ {
		if msgs[i].Role != "tool" || msgs[i].ToolCallID == "" {
			continue
		}
		call, ok := calls[msgs[i].ToolCallID]
		if !ok {
			call = agent.ToolCall{
				ID:       msgs[i].ToolCallID,
				Function: agent.ToolCallFunction{Name: msgs[i].Name},
			}
		}
		results[msgs[i].ToolCallID] = SummarizeToolResult(call, msgs[i].Content, 400)
	}
	return results, len(results)
}

func renderScopeToolActivity(msgs []agent.Message) string {
	var calls []agent.ToolCall
	results := map[string]string{}
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			for _, tc := range msgs[i].ToolCalls {
				calls = append(calls, tc)
			}
		}
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" {
			results[msgs[i].ToolCallID] = msgs[i].Content
		}
	}
	if len(calls) == 0 {
		return ""
	}
	return RenderToolActivity(calls, results)
}

func indexToolCalls(msgs []agent.Message) map[string]agent.ToolCall {
	out := make(map[string]agent.ToolCall)
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			out[call.ID] = call
		}
	}
	return out
}

// buildCompactedProjection is the exact model-facing candidate evaluated
// against the input budget: anchor + framed checkpoint + retained tail with
// persisted result summaries.
func buildCompactedProjection(anchor agent.Message, msgs []agent.Message, tailStart int, content string, retained map[string]string) []agent.Message {
	out := make([]agent.Message, 0, 2+len(msgs)-tailStart)
	out = append(out, anchor)
	out = append(out, agent.Message{Role: "user", Content: agent.FrameCompactedSummary(content)})
	for i := tailStart; i < len(msgs); i++ {
		msg := msgs[i]
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if summary := retained[msg.ToolCallID]; summary != "" {
				msg.Content = summary
			}
		}
		out = append(out, msg)
	}
	return out
}

// boundedCompactionAnchor keeps the original task anchor useful without
// allowing one oversized first message to defeat every compaction. The
// persisted message row is untouched; the checkpoint record carries only this
// model-facing excerpt and a stable retrieval note.
func boundedCompactionAnchor(anchor agent.Message, inputBudget int) (agent.Message, bool) {
	fullTokens := agent.EstimateRenderedRequest("", []agent.Message{anchor}, nil).Tokens
	budget := inputBudget / 8
	if budget > compactionAnchorMaxTokens {
		budget = compactionAnchorMaxTokens
	}
	if budget < 64 {
		budget = 64
	}
	if fullTokens <= budget {
		return anchor, false
	}
	notice := fmt.Sprintf(
		"\n\n... [first message projection excerpted; original ~%d tokens; full message remains persisted as messages/first] ...\n\n",
		fullTokens,
	)
	noticeTokens := tokens.Estimate(notice)
	bodyBudget := budget - noticeTokens
	if bodyBudget < 16 {
		bodyBudget = 16
	}

	projection := anchor
	projection.Images = nil
	projection.Content = boundedRunesToTokens(anchor.Content, bodyBudget*3/5)
	projection.ReasoningContent = boundedRunesToTokens(anchor.ReasoningContent, bodyBudget/5)

	// Historical tool calls in the anchor cannot remain protocol-valid without
	// their paired results. Preserve their names as text and omit the structural
	// calls instead of emitting truncated/invalid JSON or orphan calls.
	if len(projection.ToolCalls) > 0 {
		names := make([]string, 0, len(projection.ToolCalls))
		for _, call := range projection.ToolCalls {
			names = append(names, call.Function.Name)
		}
		projection.ToolCalls = nil
		projection.Content += "\n[omitted anchor tool calls: " + strings.Join(names, ", ") + "]"
	}
	projection.Content += notice

	// Estimator overhead can still push a pathologically ID-heavy projection
	// over the target; reduce variable fields deterministically until it fits.
	for agent.EstimateRenderedRequest("", []agent.Message{projection}, nil).Tokens > budget {
		switch {
		case projection.Content != "":
			projection.Content = boundedRunesToTokens(
				projection.Content,
				tokens.Estimate(projection.Content)*3/4,
			)
		case projection.ReasoningContent != "":
			projection.ReasoningContent = boundedRunesToTokens(
				projection.ReasoningContent,
				tokens.Estimate(projection.ReasoningContent)*3/4,
			)
		case len(projection.ToolCalls) > 0:
			projection.ToolCalls = nil
		default:
			return projection, true
		}
	}
	return projection, true
}

func boundedRunesToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if tokens.Estimate(s) <= maxTokens {
		return s
	}
	runes := []rune(s)
	keep := len(runes)
	if maxTokens*2 < keep {
		keep = maxTokens * 2
	}
	for keep > 2 {
		head := keep * 2 / 3
		tail := keep - head
		candidate := string(runes[:head]) + "…" +
			string(runes[len(runes)-tail:])
		if tokens.Estimate(candidate) <= maxTokens {
			return candidate
		}
		keep = keep * 3 / 4
	}
	return string(runes[:1])
}

// advanceTailStart removes one complete unit from the compacted middle
// (shrinking the retained tail by that unit). Tool results always move with
// the assistant tool-call that produced them.
func advanceTailStart(msgs []agent.Message, start int) int {
	if start >= len(msgs) {
		return start
	}
	next := start + 1
	for next < len(msgs) && msgs[next].Role == "tool" {
		next++
	}
	return next
}

// chooseTailForReservedBudget advances the tail boundary without calling the
// summarizer. The summary slot is represented by its maximum allowed output
// size, so one final model call is sufficient after the tail is selected.
func chooseTailForReservedBudget(in CompactionInput, anchor agent.Message, scopeStart, start int) (int, bool) {
	summaryReserve := compactionSummaryMaxTokens
	if half := in.InputBudget / 2; summaryReserve > half {
		summaryReserve = half
	}
	if summaryReserve < 128 {
		summaryReserve = 128
	}
	reservedSummary := strings.Repeat("x", summaryReserve*4)
	for {
		if start <= scopeStart || start > len(in.AgentMsgs) {
			return start, false
		}
		retained, _ := summarizeRetainedToolResults(in.AgentMsgs, start)
		content := reservedSummary
		if activity := renderScopeToolActivity(in.AgentMsgs[scopeStart:start]); activity != "" {
			content += "\n\n## Tool Activity\n" + activity
		}
		projected := buildCompactedProjection(anchor, in.AgentMsgs, start, content, retained)
		cost := in.RequestOverheadTokens + agent.EstimateRenderedRequest("", projected, nil).Tokens
		if cost <= in.InputBudget {
			return start, true
		}
		next := advanceTailStart(in.AgentMsgs, start)
		if next <= start {
			return start, false
		}
		start = next
	}
}

func measurementBasis(measured bool) string {
	if measured {
		return "provider_observed"
	}
	return "estimated"
}
