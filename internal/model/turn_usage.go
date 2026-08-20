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
	ID          int64  `db:"id"           json:"id"`
	SessionID   string `db:"session_id"   json:"session_id"`
	TraceID     string `db:"trace_id"     json:"trace_id"`
	UserID      string `db:"user_id"      json:"user_id"`
	UsageJSON   string `db:"usage_json"   json:"usage_json"`
	TotalTokens int    `db:"total_tokens" json:"total_tokens"`
	// Model mirrors turn_usage.model (migration 015,
	// per-request-model-selection): the model name this turn resolved to —
	// the request-selected catalog entry when the request carried model
	// parameters, else the deployment default. Empty is stored as NULL
	// (NULLIF at insert), matching pre-migration rows.
	Model string `db:"model" json:"model"`
	// ContextTokens mirrors turn_usage.context_tokens (migration 013): the
	// LAST LLM round's prompt+completion of the turn — the authoritative
	// end-of-turn context size the turn-start preventive compaction check
	// reads. Distinct from TotalTokens, which sums every round of the turn
	// and therefore always overstates the context size.
	ContextTokens int       `db:"context_tokens" json:"context_tokens"`
	CreatedAt     time.Time `db:"created_at"     json:"created_at"`
}
