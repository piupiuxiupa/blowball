package model

import "time"

// FileVersion is one row of the file_versions index (turn-artifacts
// capability): the metadata half of an artifact version snapshot. The content
// bytes live in the server-side blob store keyed by (UserID, Path, VersionID);
// this row is the lookup + ownership record.
type FileVersion struct {
	ID         uint64    `db:"id" json:"-"`
	UserID     string    `db:"user_id" json:"-"`
	Path       string    `db:"path" json:"path"`
	VersionID  string    `db:"version_id" json:"version_id"`
	Size       int64     `db:"size" json:"size"`
	Mime       string    `db:"mime" json:"mime"`
	SHA256     string    `db:"sha256" json:"-"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}
