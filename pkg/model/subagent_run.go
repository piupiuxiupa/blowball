package model

import (
	"errors"
	"time"
)

// ErrSubAgentChainConflict reports that another writer advanced a sub-agent
// instance's linear run chain past the predecessor observed by a dispatcher.
var ErrSubAgentChainConflict = errors.New("subagent instance chain was advanced by another run")

// Sub-agent run status values.
const (
	SubAgentStatusCompleted = "completed"
	SubAgentStatusCapped    = "capped"
	SubAgentStatusError     = "error"
)

// SubAgentSnapshotKind distinguishes rows written by the old instance-latest
// full-snapshot design from the per-run delta ledger.
const (
	SubAgentSnapshotLegacyFull = "legacy_full"
	SubAgentSnapshotDelta      = "delta"
)

// SubAgentInstance is the stable thread-level metadata for a dynamic sub-agent.
// The instance identity survives resumes; LatestRunID identifies the current
// linear chain head used to reconstruct its model context.
type SubAgentInstance struct {
	ID               int64     `db:"id"                 json:"id"`
	SessionID        string    `db:"session_id"         json:"session_id"`
	AgentInstanceID  string    `db:"agent_instance_id"  json:"agent_instance_id"`
	ParentInstanceID string    `db:"parent_instance_id" json:"parent_instance_id,omitempty"`
	Depth            int       `db:"depth"              json:"depth"`
	Name             string    `db:"name"               json:"name"`
	SystemPrompt     string    `db:"system_prompt"      json:"-"`
	ToolsJSON        []byte    `db:"tools_json"         json:"-"`
	LatestRunID      string    `db:"latest_run_id"      json:"latest_run_id,omitempty"`
	CreateTime       time.Time `db:"create_time"        json:"create_time"`
	UpdateTime       time.Time `db:"update_time"        json:"update_time"`
}

// SubAgentRun is one dynamic sub-agent execution. New rows store only the
// messages added by this run in MessagesJSON. Rows marked LegacyFull contain
// the complete context written by the previous schema and act as a baseline.
type SubAgentRun struct {
	ID                  int64     `db:"id"                    json:"id"`
	SessionID           string    `db:"session_id"           json:"session_id"`
	AgentInstanceID     string    `db:"agent_instance_id"    json:"agent_instance_id"`
	RunID               string    `db:"run_id"               json:"run_id"`
	PreviousRunID       string    `db:"previous_run_id"      json:"previous_run_id,omitempty"`
	RunNo               int       `db:"run_no"               json:"run_no"`
	ParentInstanceID    string    `db:"parent_instance_id"   json:"parent_instance_id,omitempty"`
	Depth               int       `db:"depth"                json:"depth"`
	Name                string    `db:"name"                 json:"name"`
	ToolsJSON           []byte    `db:"tools_json"           json:"-"`
	Status              string    `db:"status"               json:"status"`
	SnapshotKind        string    `db:"snapshot_kind"        json:"-"`
	ResumeEligible      bool      `db:"resume_eligible"      json:"-"`
	BaseMessageCount    int       `db:"base_message_count"   json:"-"`
	MessageCount        int       `db:"message_count"        json:"message_count"`
	ContextMessageCount int       `db:"context_message_count" json:"-"`
	ContextBytes        int       `db:"context_bytes"        json:"-"`
	MessagesJSON        []byte    `db:"messages_json"        json:"-"`
	StartedAt           time.Time `db:"started_at"           json:"started_at"`
	FinishedAt          time.Time `db:"finished_at"          json:"finished_at"`
	CreateTime          time.Time `db:"create_time"          json:"create_time"`
	UpdateTime          time.Time `db:"update_time"          json:"update_time"`
}

// SubAgentHistory is the ordered run chain belonging to one instance.
type SubAgentHistory struct {
	Instance SubAgentInstance `json:"instance"`
	Runs     []SubAgentRun    `json:"runs"`
}
