package model

import "time"

// Message role values. These match the `role` column on the messages table.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Agent names. AgentUser and AgentConfucius remain active wire labels.
// AgentChongzhi/AgentLiang are retained for decoding legacy rows; dynamic
// sub-agents emit operator/model supplied instance labels instead.
const (
	AgentUser      = "user"
	AgentConfucius = "Confucius"
	AgentChongzhi  = "Chongzhi"
	AgentLiang     = "Liang"
)

// Event type values. These match the `event_type` column on the messages table.
const (
	EventTypeMessage     = "message"
	EventTypeToken       = "token"
	EventTypeReasoning   = "reasoning"
	EventTypeToolCall    = "tool_call"
	EventTypeToolResult  = "tool_result"
	EventTypePlanUpdated = "plan_updated"
	EventTypeAgentStart  = "agent_start"
	EventTypeAgentEnd    = "agent_end"
	EventTypeAgentError  = "agent_error"
)

// Message mirrors the `messages` table (migration 004_messages.sql,
// 005_messages_event_type.sql, 012_client_msg_id.sql and
// 016_dynamic_subagents.sql).
type Message struct {
	ID        int64     `db:"id"          json:"id"`
	SessionID string    `db:"session_id"  json:"session_id"`
	MsgTime   time.Time `db:"msg_time"    json:"msg_time"`
	Agent     string    `db:"agent"       json:"agent"`
	MsgIndex  int       `db:"msg_index"   json:"msg_index"`
	Role      string    `db:"role"        json:"role"`
	EventType string    `db:"event_type"  json:"event_type"`
	Content   string    `db:"content"     json:"content"`
	TraceID   string    `db:"trace_id"    json:"trace_id"`
	// ClientMsgID is the idempotency key minted at persistence time (UUID): the
	// write-behind queue may redeliver a message (crash recovery, retry,
	// fallback direct-write), and the messages table's UNIQUE index on
	// client_msg_id plus INSERT IGNORE collapses redelivery to one row.
	// Legacy rows written before migration 012 carry NULL / an empty string.
	ClientMsgID string `db:"client_msg_id" json:"client_msg_id"`
	// RunID is the sub-agent invocation run identity
	// (subagent-run-identity capability): the parent invoke_* tool_call id
	// that produced this event, copied from the event's
	// Meta.parent_tool_call_id at persistence time. It is set only on rows
	// emitted by a sub-agent Run — Confucius's own events and user rows carry
	// NULL/empty, and legacy rows (before migration 014) are NULL. Rows keep
	// arrival order (msg_time, msg_index); consumers regroup interleaved rows
	// by (agent, run_id).
	// AgentInstanceID is the stable identity of the sub-agent instance that
	// produced this event. Unlike RunID it survives resume; UI threads group
	// by (Agent, AgentInstanceID) and use RunID to split individual runs.
	RunID           string    `db:"run_id"            json:"run_id,omitempty"`
	AgentInstanceID string    `db:"agent_instance_id" json:"agent_instance_id,omitempty"`
	UpdateTime      time.Time `db:"update_time"       json:"update_time"`
}
