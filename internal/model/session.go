package model

import "time"

// SessionSource identifies which physically separate table group a
// conversation lives in. It is selected server-side from the authenticated
// route or execution identity — never from a client-supplied table/path name —
// and there is no cross-table fallback when a row is missing.
type SessionSource string

const (
	// SessionSourceInteractive is the ordinary chat surface (`sessions`,
	// `messages`, `tool_calls`, ...).
	SessionSourceInteractive SessionSource = "interactive"
	// SessionSourceScheduled is the scheduled-task conversation group
	// (`scheduled_sessions`, `scheduled_messages`, ...).
	SessionSourceScheduled SessionSource = "scheduled"
	// SessionSourceLegacyInteractive marks a pre-split scheduled occurrence
	// whose content was written into an ordinary session. Reads route through
	// the ordinary tables by explicit provenance; nothing is copied.
	SessionSourceLegacyInteractive SessionSource = "legacy_interactive"
)

// Valid reports whether s is a known source.
func (s SessionSource) Valid() bool {
	switch s {
	case SessionSourceInteractive, SessionSourceScheduled, SessionSourceLegacyInteractive:
		return true
	default:
		return false
	}
}

// UsesScheduledTables reports whether the source's conversation rows live in
// the scheduled table group.
func (s SessionSource) UsesScheduledTables() bool {
	return s == SessionSourceScheduled
}

// Normalized maps the empty value to the interactive default so callers that
// predate the source split keep their exact behavior.
func (s SessionSource) Normalized() SessionSource {
	if s == "" {
		return SessionSourceInteractive
	}
	return s
}

// Session mirrors the `sessions` table (migration 002_sessions.sql) and, for
// scheduled conversations, the separate `scheduled_sessions` table
// (migration 028). The TaskID/OccurrenceID/RunID fields are populated only for
// the scheduled source; Source is a derived, never client-supplied marker.
type Session struct {
	SessionID  string    `db:"session_id"  json:"session_id"`
	UserID     string    `db:"user_id"     json:"user_id"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
	// ContextCompacted mirrors sessions.context_compacted (migration 013):
	// 1 once a context compaction has completed for this session. It gates
	// stitched-context reconstruction on subsequent turns and is never reset
	// to 0.
	ContextCompacted bool `db:"context_compacted" json:"context_compacted"`

	// Scheduled-task linkage (scheduled_sessions only).
	TaskID       string `db:"task_id"       json:"task_id,omitempty"`
	OccurrenceID string `db:"occurrence_id" json:"occurrence_id,omitempty"`
	RunID        string `db:"run_id"        json:"run_id,omitempty"`

	// Source is derived from the table group that produced the row. It is
	// emitted on scheduled-session DTOs so historical occurrences can state
	// their provenance explicitly.
	Source SessionSource `db:"-" json:"session_source,omitempty"`
}

// HydrateSource stamps the row's source. Store reads call this after scanning
// so no caller can mistake a scheduled row for an ordinary one (or vice versa).
func (s *Session) HydrateSource(source SessionSource) {
	if s != nil {
		s.Source = source
	}
}
