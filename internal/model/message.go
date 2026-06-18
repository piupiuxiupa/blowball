package model

import "time"

// Message role values. These match the `role` column on the messages table.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Agent names. These match the `agent` column on the messages table and are
// emitted in StreamEvent.Agent. The special value AgentUser is used for rows
// produced by the end user rather than an assistant agent.
const (
	AgentUser     = "user"
	AgentConfuse  = "Confuse"
	AgentChongzhi = "Chongzhi"
	AgentLiang    = "Liang"
)

// Event type values. These match the `event_type` column on the messages table.
const (
	EventTypeMessage    = "message"
	EventTypeToken      = "token"
	EventTypeToolCall   = "tool_call"
	EventTypeToolResult = "tool_result"
	EventTypeAgentStart = "agent_start"
	EventTypeAgentEnd   = "agent_end"
	EventTypeAgentError = "agent_error"
)

// Message mirrors the `messages` table (migration 004_messages.sql and
// 005_messages_event_type.sql).
type Message struct {
	ID         int64     `db:"id"          json:"id"`
	SessionID  string    `db:"session_id"  json:"session_id"`
	MsgTime    time.Time `db:"msg_time"    json:"msg_time"`
	Agent      string    `db:"agent"       json:"agent"`
	MsgIndex   int       `db:"msg_index"   json:"msg_index"`
	Role       string    `db:"role"        json:"role"`
	EventType  string    `db:"event_type"  json:"event_type"`
	Content    string    `db:"content"     json:"content"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
}
