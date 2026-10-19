package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// insertAttachmentSQL registers one uploaded chat image. The id is the UUIDv7
// minted by the handler (also the on-disk filename stem), so the insert is
// naturally idempotent on the primary key.
const insertAttachmentSQL = `
INSERT INTO attachments (id, user_id, session_id, filename, mime, size)
VALUES (:id, :user_id, :session_id, :filename, :mime, :size)
`

// getAttachmentSQL resolves the ownership triple (id + user + session) in one
// query — the send-time validation primitive. A miss returns (nil, nil).
const getAttachmentSQL = `
SELECT id, user_id, session_id, filename, mime, size, message_id, create_time
FROM attachments
WHERE id = ? AND user_id = ? AND session_id = ?
`

// InsertAttachment registers a in the attachments table.
func (s *Store) InsertAttachment(ctx context.Context, a model.Attachment) error {
	logQuery(ctx, "attachment.insert", insertAttachmentSQL, a.ID)
	params := map[string]any{
		"id":         a.ID,
		"user_id":    a.UserID,
		"session_id": a.SessionID,
		"filename":   a.Filename,
		"mime":       a.MIME,
		"size":       a.Size,
	}
	if _, err := sqlx.NamedExecContext(ctx, s.db, insertAttachmentSQL, params); err != nil {
		return fmt.Errorf("mysql.InsertAttachment: %w", err)
	}
	return nil
}

// GetAttachment returns the attachment row for the (id, userID, sessionID)
// ownership triple, or (nil, nil) when no such row exists — the store-wide
// not-found convention. Callers treat a nil row as 404; the triple match
// never leaks whether the id exists under another user/session.
func (s *Store) GetAttachment(ctx context.Context, id, userID, sessionID string) (*model.Attachment, error) {
	logQuery(ctx, "attachment.get", getAttachmentSQL, id, userID, sessionID)
	var a model.Attachment
	err := s.db.GetContext(ctx, &a, getAttachmentSQL, id, userID, sessionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("mysql.GetAttachment: %w", err)
	}
	return &a, nil
}
