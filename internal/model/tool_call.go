package model

import "time"

// Tool-call row status values (tool_calls.status).
const (
	ToolCallStatusPending   = "pending"
	ToolCallStatusCompleted = "completed"
)

// Message final-row status values (messages.status).
const (
	MessageStatusInterrupted = "interrupted"
)

// ToolCall mirrors the `tool_calls` table (migration 020_tool_calls.sql): the
// canonical record of one tool interaction, pairing the call with its result
// on a single row. Reconstruction joins it onto the assistant final row of
// the same (turn, round).
type ToolCall struct {
	ID         int64     `db:"id"           json:"id"`
	SessionID  string    `db:"session_id"   json:"session_id"`
	TurnID     string    `db:"turn_id"      json:"turn_id"`
	RoundNo    int       `db:"round_no"     json:"round_no"`
	ToolCallID string    `db:"tool_call_id" json:"tool_call_id"`
	Name       string    `db:"name"         json:"name"`
	Arguments  string    `db:"arguments"    json:"arguments"`
	Result     string    `db:"result"       json:"result"`
	Status     string    `db:"status"       json:"status"`
	CreateTime time.Time `db:"create_time"  json:"create_time"`
	UpdateTime time.Time `db:"update_time"  json:"update_time"`
}
