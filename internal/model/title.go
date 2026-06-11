package model

import "time"

// Title mirrors the `titles` table (migration 003_titles.sql).
type Title struct {
	SessionID  string    `db:"session_id"  json:"session_id"`
	Title      string    `db:"title"       json:"title"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}
