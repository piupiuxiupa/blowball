package model

import "time"

// ScheduledResourceDraft is the per-task `.cron` draft fence row. Every
// authorized draft mutation bumps DraftRevision; publication snapshots it and
// aborts if it changed during the copy.
type ScheduledResourceDraft struct {
	TaskID        string    `db:"task_id"`
	UserID        string    `db:"user_id"`
	DraftRevision uint64    `db:"draft_revision"`
	Generation    string    `db:"generation"`
	UpdateTime    time.Time `db:"update_time"`
	CreateTime    time.Time `db:"create_time"`
}

// ScheduledResourceVersion is one immutable published resource revision.
type ScheduledResourceVersion struct {
	TaskID           string    `db:"task_id"`
	Revision         uint64    `db:"resource_revision"`
	UserID           string    `db:"user_id"`
	ManifestHash     string    `db:"manifest_hash"`
	FileCount        int       `db:"file_count"`
	TotalBytes       int64     `db:"total_bytes"`
	ManifestJSON     string    `db:"manifest_json"`
	RequestID        string    `db:"request_id"`
	CreateTime       time.Time `db:"create_time"`
}
