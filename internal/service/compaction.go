package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// Fixed context-compaction parameters (context-compaction capability). The
// trigger threshold is a hard-coded 80% of openai.max_context_tokens —
// intentionally NOT configurable (design D-risk: keep it one constant so a
// future config knob is a one-line change); retainedTail is the number of
// trailing agent messages kept verbatim on compaction.
const (
	compactionThresholdNumerator   = 8
	compactionThresholdDenominator = 10
	compactionRetainedTail         = 5
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
// The service is inert while openai.max_context_tokens is unset (0): Enabled
// reports false and callers skip every check, reproducing the pre-capability
// behavior byte for byte.
type CompactionService struct {
	deps      SessionDeps
	llm       agent.LLMClient
	confucius config.AgentConfig
	// maxContextTokens is openai.max_context_tokens; 0 disables compaction.
	maxContextTokens int
}

// NewCompactionService wires a CompactionService from the shared store deps,
// the LLM client (the same OpenAIClient the agents use, so summary calls land
// in llm_raw_log like any other call), the Confucius agent config (the
// summary reuses its model — no separate summarizer model by design), and the
// configured max context window.
func NewCompactionService(deps SessionDeps, llm agent.LLMClient, confucius config.AgentConfig, maxContextTokens int) *CompactionService {
	return &CompactionService{
		deps:             deps,
		llm:              llm,
		confucius:        confucius,
		maxContextTokens: maxContextTokens,
	}
}

// Enabled reports whether compaction is configured at all. Zero (unset
// max_context_tokens) means disabled: no trigger checks fire anywhere.
func (s *CompactionService) Enabled() bool {
	return s != nil && s.maxContextTokens > 0
}

// ThresholdTokens returns the absolute trigger threshold (80% of the
// configured max context). Integer math: the comparison in ShouldCompact is
// tokens*10 >= max*8, so an exact tie at the threshold triggers.
func (s *CompactionService) ThresholdTokens() int {
	if !s.Enabled() {
		return 0
	}
	return s.maxContextTokens * compactionThresholdNumerator / compactionThresholdDenominator
}

// ShouldCompact reports whether the measured context pressure (the
// authoritative prompt+completion of the last LLM round, or the latest
// turn_usage.context_tokens for turn-start checks) has reached the trigger
// threshold. Disabled service → always false.
func (s *CompactionService) ShouldCompact(contextTokens int) bool {
	if !s.Enabled() {
		return false
	}
	return contextTokens*compactionThresholdDenominator >= s.maxContextTokens*compactionThresholdNumerator
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
func (s *CompactionService) RecoverDurable(ctx context.Context, sessionID string) ([]model.Message, error) {
	if s.deps.DrainMessageQueue != nil {
		drainCtx, cancel := context.WithTimeout(ctx, messageDrainTimeout)
		err := s.deps.DrainMessageQueue(drainCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("compaction.recover_durable: drain: %w", err)
		}
	}
	return s.deps.MySQL.ListMessages(ctx, sessionID)
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

	// Prior record: merged into the summary input on re-compaction. A read
	// failure degrades to "no prior" (the summary then re-covers the whole
	// middle — a safe superset, never a broken state).
	prior := s.LatestCompactionRecord(ctx, in.SessionID)

	scopeStart := 1
	if prior != nil {
		scopeStart = scopeStartAfterBoundary(in.Rows, in.AgentMsgs, in.LastRow, prior)
	}

	// Retained tail: the last N agent messages, extended backward so the cut
	// never separates an assistant tool-call message from its tool results.
	tailStart := len(in.AgentMsgs) - compactionRetainedTail
	if tailStart < 0 {
		tailStart = 0
	}
	for tailStart > scopeStart && in.AgentMsgs[tailStart].Role == "tool" {
		tailStart--
	}
	if tailStart <= scopeStart {
		log.Debug("compaction skipped: compressible middle is empty")
		return nil, false
	}

	middle := in.AgentMsgs[scopeStart:tailStart]
	boundaryRow := in.Rows[in.LastRow[tailStart-1]]

	content, usage, ok := s.summarize(ctx, log, prior, middle)
	if !ok {
		return nil, false
	}

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
		// The summary prompt IS the shadowed span (+ prior checkpoint +
		// instruction), so its prompt size is the faithful measured proxy for
		// the shadowed content.
		ShadowedTokens:          usage.PromptTokens,
		SummaryModel:            s.confucius.Model,
		SummaryPromptTokens:     usage.PromptTokens,
		SummaryCompletionTokens: usage.CompletionTokens,
	}

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
		zap.Int("shadowed_messages", len(middle)),
		zap.Int64("boundary_msg_id", rec.BoundaryMsgID),
		zap.Int("summary_prompt_tokens", rec.SummaryPromptTokens),
		zap.Int("summary_completion_tokens", rec.SummaryCompletionTokens))
	return &rec, true
}

// summarize runs the checkpoint LLM call over the shadowed middle, merging a
// prior checkpoint when the session was compacted before. Only the returned
// text is retained; reasoning content and tool calls are discarded (never
// persisted into the record). The call itself flows through the ordinary
// OpenAI client, so raw capture (llm_raw_log) observes it for free.
func (s *CompactionService) summarize(ctx context.Context, log *zap.Logger, prior *model.ContextCompaction, middle []agent.Message) (string, agent.Usage, bool) {
	if s.llm == nil || s.confucius.Model == "" {
		log.Warn("compaction summarizer unavailable (no llm client or model); skipping compaction")
		return "", agent.Usage{}, false
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
		Model: s.confucius.Model,
		Messages: []agent.Message{
			{Role: "system", Content: compactionSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		MaxTokens: s.confucius.MaxTokens,
	}

	resp, err := s.llm.StreamChat(agentCtx, req, nil, nil)
	if err != nil {
		log.Warn("compaction summary llm call failed; continuing with original context", zap.Error(err))
		return "", agent.Usage{}, false
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" {
		log.Warn("compaction summary empty; continuing with original context")
		return "", agent.Usage{}, false
	}
	return content, resp.Usage, true
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
