package model

import "time"

// TurnUsage mirrors the `turn_usage` table (migration 010_turn_usage.sql). One
// row records the per-agent token cost of a single completed chat turn,
// identified by (session_id, trace_id). UsageJSON holds the authoritative
// usage object emitted on the SSE done event — {total, by_agent, meta} (see
// the turn-cost-tracking spec) — and TotalTokens redundantly stores the
// aggregate total so per-session cost can be summed without parsing JSON.
//
// Rows cascade with session deletion via the FK on session_id.
type TurnUsage struct {
	ID          int64     `db:"id"           json:"id"`
	SessionID   string    `db:"session_id"   json:"session_id"`
	TraceID     string    `db:"trace_id"     json:"trace_id"`
	UserID      string    `db:"user_id"      json:"user_id"`
	UsageJSON   string    `db:"usage_json"   json:"usage_json"`
	TotalTokens int       `db:"total_tokens" json:"total_tokens"`
	CreatedAt   time.Time `db:"created_at"   json:"created_at"`
}
