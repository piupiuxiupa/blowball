package model

import "time"

// Message role values. These match the `role` column on the messages table.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Agent names. These match the `agent` column on the messages table and are
// emitted in StreamEvent.Agent.
const (
	AgentConfuse  = "Confuse"
	AgentChongzhi = "Chongzhi"
	AgentLiang    = "Liang"
)

// Message mirrors the `messages` table (migration 004_messages.sql).
type Message struct {
	ID         int64     `db:"id"          json:"id"`
	SessionID  string    `db:"session_id"  json:"session_id"`
	MsgTime    time.Time `db:"msg_time"    json:"msg_time"`
	Agent      string    `db:"agent"       json:"agent"`
	MsgIndex   int       `db:"msg_index"   json:"msg_index"`
	Role       string    `db:"role"        json:"role"`
	Content    string    `db:"content"     json:"content"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
}
