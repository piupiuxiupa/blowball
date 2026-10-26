package model

import (
	"encoding/json"
	"time"
)

// Context-compaction trigger kinds (context_compactions.trigger_kind).
const (
	CompactionTriggerMidTurn   = "mid_turn"
	CompactionTriggerTurnStart = "turn_start"
)

// ContextCompaction mirrors the `context_compactions` table (migrations
// 013_context_compaction.sql, 023_context_compaction_budget.sql, and
// 024_context_compaction_anchor.sql). One row
// records one completed context compaction: the checkpoint summary that
// replaced a shadowed span of history,
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
	Content          string    `db:"content"                    json:"content"`
	BoundaryMsgTime  time.Time `db:"boundary_msg_time"          json:"boundary_msg_time"`
	BoundaryMsgIndex int       `db:"boundary_msg_index"         json:"boundary_msg_index"`
	BoundaryMsgID    int64     `db:"boundary_msg_id"            json:"boundary_msg_id"`
	// AnchorJSON is the complete bounded agent.Message projection when the
	// original first message is too large. Empty + AnchorTruncated=false means
	// legacy/full anchor behavior.
	AnchorJSON              string `db:"anchor_json"                 json:"anchor_json,omitempty"`
	AnchorTruncated         bool   `db:"anchor_truncated"            json:"anchor_truncated,omitempty"`
	ShadowedTokens          int    `db:"shadowed_tokens"            json:"shadowed_tokens"`
	SummaryModel            string `db:"summary_model"              json:"summary_model"`
	SummaryPromptTokens     int    `db:"summary_prompt_tokens"      json:"summary_prompt_tokens"`
	SummaryCompletionTokens int    `db:"summary_completion_tokens"  json:"summary_completion_tokens"`
	// RetainedResults holds deterministic tool-result summaries for the
	// retained tail, keyed by tool_call_id. It is JSON because call ids are
	// provider-generated strings and the tail can contain parallel calls.
	RetainedResultsJSON  []byte            `db:"retained_results_json" json:"-"`
	RetainedResults      map[string]string `db:"-" json:"retained_results,omitempty"`
	MeasurementBasis     string            `db:"measurement_basis" json:"measurement_basis,omitempty"`
	PreCompactionTokens  int               `db:"pre_compaction_tokens" json:"pre_compaction_tokens,omitempty"`
	InputBudgetTokens    int               `db:"input_budget_tokens" json:"input_budget_tokens,omitempty"`
	PostCompactionTokens int               `db:"post_compaction_tokens" json:"post_compaction_tokens,omitempty"`
	RetainedTailMessages int               `db:"retained_tail_messages" json:"retained_tail_messages,omitempty"`
	SummaryStrategy      string            `db:"summary_strategy" json:"summary_strategy,omitempty"`
	ToolSummaryCount     int               `db:"tool_summary_count" json:"tool_summary_count,omitempty"`
	ToolActivityCount    int               `db:"tool_activity_count" json:"tool_activity_count,omitempty"`
	CreateTime           time.Time         `db:"create_time"                json:"create_time"`
}

// DecodeRetainedResults hydrates RetainedResults from its on-disk JSON column.
// A nil/empty or malformed value degrades to nil so legacy records remain
// readable and callers can fall back to raw reconstructed tail messages.
func (c *ContextCompaction) DecodeRetainedResults() {
	if c == nil || len(c.RetainedResultsJSON) == 0 {
		c.RetainedResults = nil
		return
	}
	var out map[string]string
	if err := json.Unmarshal(c.RetainedResultsJSON, &out); err != nil {
		c.RetainedResults = nil
		return
	}
	c.RetainedResults = out
}
