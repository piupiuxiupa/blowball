package model

import "time"

// Session mirrors the `sessions` table (migration 002_sessions.sql).
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
}
