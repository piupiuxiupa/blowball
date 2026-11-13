package model

import "time"

// Title mirrors the `titles` table (migration 003_titles.sql + 009_titles_manual.sql).
type Title struct {
	SessionID  string    `db:"session_id"  json:"session_id"`
	Title      string    `db:"title"       json:"title"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	IsManual   bool      `db:"is_manual"   json:"is_manual"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}
