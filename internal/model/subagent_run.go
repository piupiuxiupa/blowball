package model

import "time"

// Sub-agent run snapshot status values.
const (
	SubAgentStatusCompleted = "completed"
	SubAgentStatusCapped    = "capped"
	SubAgentStatusError     = "error"
)

// SubAgentRun is one dynamic sub-agent instance's authoritative persisted
// state. The row is upserted by (session_id, agent_instance_id) at the end of
// every run; resume starts from MessagesJSON rather than reconstructing chat
// history from display events.
type SubAgentRun struct {
	ID               int64     `db:"id"                 json:"id"`
	SessionID        string    `db:"session_id"         json:"session_id"`
	AgentInstanceID  string    `db:"agent_instance_id"  json:"agent_instance_id"`
	ParentInstanceID string    `db:"parent_instance_id" json:"parent_instance_id,omitempty"`
	Depth            int       `db:"depth"              json:"depth"`
	Name             string    `db:"name"               json:"name"`
	ToolsJSON        []byte    `db:"tools_json"         json:"tools_json,omitempty"`
	Status           string    `db:"status"             json:"status"`
	ResumeEligible   bool      `db:"resume_eligible"    json:"resume_eligible"`
	MessagesJSON     []byte    `db:"messages_json"      json:"messages_json"`
	CreateTime       time.Time `db:"create_time"        json:"create_time"`
	UpdateTime       time.Time `db:"update_time"        json:"update_time"`
}
