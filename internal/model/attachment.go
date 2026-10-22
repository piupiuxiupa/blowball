package model

import (
	"encoding/json"
	"time"
)

// Attachment mirrors the `attachments` registry table (migrations
// 021_image_attachments.sql and 022_staged_attachments.sql): one row per
// uploaded chat image. The registry owns upload-time metadata, the ownership
// check used at send time, and the session-delete lifecycle. Image BYTES live
// only on disk under data/{user}/attachments/{session}/{id}.{ext} — base64 is
// never persisted anywhere and is computed from the immutable file at request
// time.
//
// SessionID is nil while the attachment is STAGED (add-staged-attachment-
// upload): uploaded via POST /api/v1/attachments before any session exists,
// parked under data/{user}/attachments/_staging/, and claimed into a session
// at send time. A non-nil SessionID is the owning (claimed) session.
type Attachment struct {
	ID        string  `db:"id"          json:"id"`
	UserID    string  `db:"user_id"     json:"-"`
	SessionID *string `db:"session_id"  json:"-"`
	Filename  string  `db:"filename"    json:"filename"`
	MIME      string  `db:"mime"        json:"mime"`
	Size      int64   `db:"size"        json:"size"`
	// MessageID is a best-effort backfill reserved for orphan-cleanup stats
	// (uploaded but never sent). The authoritative message linkage is the
	// attachments_json reference list on the message row itself.
	MessageID  *int64    `db:"message_id"  json:"-"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
}

// MessageAttachmentRef is one image reference embedded in a message row's
// attachments_json column. Filename/MIME are denormalized from the registry
// row at send time — attachment metadata is immutable, so the copy can never
// drift and history reads never need a join.
type MessageAttachmentRef struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MIME     string `json:"mime"`
}

// MarshalAttachmentRefs renders refs as the attachments_json column payload.
// It returns nil for an empty list so non-attachment rows keep writing SQL
// NULL (the pre-change column semantics).
func MarshalAttachmentRefs(refs []MessageAttachmentRef) []byte {
	if len(refs) == 0 {
		return nil
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return nil
	}
	return b
}

// UnmarshalAttachmentRefs parses an attachments_json payload. NULL/empty and
// malformed values both yield nil — a corrupt reference list must never break
// history reads (the placeholder renderer simply sees no attachments).
func UnmarshalAttachmentRefs(b []byte) []MessageAttachmentRef {
	if len(b) == 0 {
		return nil
	}
	var refs []MessageAttachmentRef
	if err := json.Unmarshal(b, &refs); err != nil {
		return nil
	}
	return refs
}
