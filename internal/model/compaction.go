package model

import "time"

// Context-compaction trigger kinds (context_compactions.trigger_kind).
const (
	CompactionTriggerMidTurn   = "mid_turn"
	CompactionTriggerTurnStart = "turn_start"
)

// ContextCompaction mirrors the `context_compactions` table (migration
// 013_context_compaction.sql). One row records one completed context
// compaction: the checkpoint summary that replaced a shadowed span of history,
// the composite cursor of the last shadowed messages row (the stitch boundary),
// and the observation columns for the summarization call.
//
// The table is append-only with latest-wins semantics: every compaction
// inserts a row, stitching reads the (session_id, id)-max row, and historical
// rows remain as an audit trail. Rows cascade with session deletion via the
// FK on session_id (the turn_usage precedent) and are deliberately NOT copied
// into the 008 deletion-archive mirrors.
type ContextCompaction struct {
	ID            int64  `db:"id"                         json:"id"`
	SessionID     string `db:"session_id"                 json:"session_id"`
	UserID        string `db:"user_id"                    json:"user_id"`
	TraceID       string `db:"trace_id"                   json:"trace_id"`
	TriggerKind   string `db:"trigger_kind"               json:"trigger_kind"`
	TriggerTokens int    `db:"trigger_tokens"             json:"trigger_tokens"`
	// Content is the BARE summary text. The <compacted-summary> framing and
	// lead-in are applied at stitch time (a prompt-engineering concern), so
	// the stored value stays reusable across framing changes.
	Content                 string    `db:"content"                    json:"content"`
	BoundaryMsgTime         time.Time `db:"boundary_msg_time"          json:"boundary_msg_time"`
	BoundaryMsgIndex        int       `db:"boundary_msg_index"         json:"boundary_msg_index"`
	BoundaryMsgID           int64     `db:"boundary_msg_id"            json:"boundary_msg_id"`
	ShadowedTokens          int       `db:"shadowed_tokens"            json:"shadowed_tokens"`
	SummaryModel            string    `db:"summary_model"              json:"summary_model"`
	SummaryPromptTokens     int       `db:"summary_prompt_tokens"      json:"summary_prompt_tokens"`
	SummaryCompletionTokens int       `db:"summary_completion_tokens"  json:"summary_completion_tokens"`
	CreateTime              time.Time `db:"create_time"                json:"create_time"`
}
