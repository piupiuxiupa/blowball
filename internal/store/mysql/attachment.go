package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

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

// getAttachmentByUserSQL resolves the (id + user) ownership pair used by the
// send path (add-staged-attachment-upload): the session dimension is answered
// by the row itself — nil SessionID means staged (claimable), a matching
// SessionID means already bound to this session, anything else means another
// session's attachment. A miss returns (nil, nil).
const getAttachmentByUserSQL = `
SELECT id, user_id, session_id, filename, mime, size, message_id, create_time
FROM attachments
WHERE id = ? AND user_id = ?
`

// claimAttachmentSQL atomically binds a staged row to a session
// (add-staged-attachment-upload). The session_id IS NULL predicate makes the
// single UPDATE the claim arbiter: exactly one concurrent claimant can win,
// and a row already bound to any session is untouchable.
const claimAttachmentSQL = `
UPDATE attachments SET session_id = ?
WHERE id = ? AND user_id = ? AND session_id IS NULL
`

// unclaimAttachmentSQL rolls a just-won claim back to staged after the file
// rename failed (add-staged-attachment-upload). The session predicate pins
// the rollback to the exact claim this caller holds, so a row that moved on
// is never clobbered.
const unclaimAttachmentSQL = `
UPDATE attachments SET session_id = NULL
WHERE id = ? AND user_id = ? AND session_id = ?
`

// countStagedAttachmentsSQL backs the per-user staged-upload quota
// (add-staged-attachment-upload): only unbound rows count.
const countStagedAttachmentsSQL = `
SELECT COUNT(*) FROM attachments
WHERE user_id = ? AND session_id IS NULL
`

const (
	stagedAttachmentQuotaLockPrefix = "blowball:staged-attachments:"
	stagedAttachmentQuotaLockWait   = 2
)

// listExpiredStagedAttachmentsSQL feeds the orphan sweeper
// (add-staged-attachment-upload): staged rows older than the cutoff, oldest
// first, bounded per sweep round.
const listExpiredStagedAttachmentsSQL = `
SELECT id, user_id, session_id, filename, mime, size, message_id, create_time
FROM attachments
WHERE session_id IS NULL AND create_time < ?
ORDER BY create_time
LIMIT ?
`

// deleteStagedAttachmentSQL removes one staged row — but only while it is
// STILL staged. The session_id IS NULL guard makes the delete lose the race
// against a concurrent claim, so the sweeper never removes a row a send just
// bound (the row delete strictly precedes the file delete for the same
// reason).
const deleteStagedAttachmentSQL = `
DELETE FROM attachments
WHERE id = ? AND session_id IS NULL
`

// restoreStagedAttachmentSQL recreates the registry row after a successful
// guarded delete whose staging-file removal failed. Preserving create_time
// makes the row immediately eligible for the next sweep, keeping cleanup
// retryable instead of leaving an untracked file behind.
const restoreStagedAttachmentSQL = `
INSERT INTO attachments (id, user_id, session_id, filename, mime, size, message_id, create_time)
VALUES (:id, :user_id, NULL, :filename, :mime, :size, :message_id, :create_time)
ON DUPLICATE KEY UPDATE id = VALUES(id)
`

// GetAttachmentByUser returns the attachment row for the (id, userID)
// ownership pair regardless of session binding, or (nil, nil) when no such
// row exists. Callers branch on SessionID: nil = staged, equal = bound to
// this session, otherwise = another session's (treated as 404).
func (s *Store) GetAttachmentByUser(ctx context.Context, id, userID string) (*model.Attachment, error) {
	logQuery(ctx, "attachment.get_by_user", getAttachmentByUserSQL, id, userID)
	var a model.Attachment
	err := s.db.GetContext(ctx, &a, getAttachmentByUserSQL, id, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("mysql.GetAttachmentByUser: %w", err)
	}
	return &a, nil
}

// ClaimAttachment atomically binds a staged attachment to sessionID. It
// reports whether the claim was won (exactly one row transitioned from
// staged to bound); a lost race, an unknown id, a foreign owner, or an
// already-bound row all report (false, nil).
func (s *Store) ClaimAttachment(ctx context.Context, id, userID, sessionID string) (bool, error) {
	logQuery(ctx, "attachment.claim", claimAttachmentSQL, id, userID, sessionID)
	res, err := s.db.ExecContext(ctx, claimAttachmentSQL, sessionID, id, userID)
	if err != nil {
		return false, fmt.Errorf("mysql.ClaimAttachment: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mysql.ClaimAttachment: rows affected: %w", err)
	}
	return affected == 1, nil
}

// UnclaimAttachment best-effort rolls a claim back to staged after the file
// rename failed. The session predicate pins the rollback to the caller's own
// claim; a mismatch is a no-op (the row moved on and must not be clobbered).
func (s *Store) UnclaimAttachment(ctx context.Context, id, userID, sessionID string) error {
	logQuery(ctx, "attachment.unclaim", unclaimAttachmentSQL, id, userID, sessionID)
	if _, err := s.db.ExecContext(ctx, unclaimAttachmentSQL, id, userID, sessionID); err != nil {
		return fmt.Errorf("mysql.UnclaimAttachment: %w", err)
	}
	return nil
}

// CountStagedAttachments returns how many staged (unbound) attachments the
// user currently holds — the staged-upload quota input.
func (s *Store) CountStagedAttachments(ctx context.Context, userID string) (int, error) {
	logQuery(ctx, "attachment.count_staged", countStagedAttachmentsSQL, userID)
	var n int
	if err := s.db.GetContext(ctx, &n, countStagedAttachmentsSQL, userID); err != nil {
		return 0, fmt.Errorf("mysql.CountStagedAttachments: %w", err)
	}
	return n, nil
}

// InsertStagedAttachment atomically reserves one staged-attachment slot for a
// user. A MySQL named lock serializes count+insert across service instances;
// the caller writes the file only after the slot is reserved.
func (s *Store) InsertStagedAttachment(ctx context.Context, a model.Attachment, limit int) (bool, error) {
	conn, err := s.db.Connx(ctx)
	if err != nil {
		return false, fmt.Errorf("mysql.InsertStagedAttachment connection: %w", err)
	}
	defer conn.Close()

	sum := sha256.Sum256([]byte(a.UserID))
	lockName := stagedAttachmentQuotaLockPrefix + hex.EncodeToString(sum[:16])
	var acquired sql.NullInt64
	if err := conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, ?)", lockName, stagedAttachmentQuotaLockWait); err != nil {
		return false, fmt.Errorf("mysql.InsertStagedAttachment acquire lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return false, fmt.Errorf("mysql.InsertStagedAttachment lock timeout for user %s", a.UserID)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", lockName)
	}
	defer release()

	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("mysql.InsertStagedAttachment begin: %w", err)
	}
	defer tx.Rollback()

	if limit > 0 {
		var staged int
		if err := tx.GetContext(ctx, &staged, countStagedAttachmentsSQL, a.UserID); err != nil {
			return false, fmt.Errorf("mysql.InsertStagedAttachment count: %w", err)
		}
		if staged >= limit {
			return false, nil
		}
	}
	logQuery(ctx, "attachment.insert_staged", insertAttachmentSQL, a.ID)
	params := map[string]any{
		"id":         a.ID,
		"user_id":    a.UserID,
		"session_id": a.SessionID,
		"filename":   a.Filename,
		"mime":       a.MIME,
		"size":       a.Size,
	}
	if _, err := tx.NamedExecContext(ctx, insertAttachmentSQL, params); err != nil {
		return false, fmt.Errorf("mysql.InsertStagedAttachment insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("mysql.InsertStagedAttachment commit: %w", err)
	}
	release()
	return true, nil
}

// ListExpiredStagedAttachments returns up to limit staged rows created before
// cutoff, oldest first — one orphan-sweeper batch.
func (s *Store) ListExpiredStagedAttachments(ctx context.Context, cutoff time.Time, limit int) ([]model.Attachment, error) {
	logQuery(ctx, "attachment.list_expired_staged", listExpiredStagedAttachmentsSQL, cutoff)
	var rows []model.Attachment
	if err := s.db.SelectContext(ctx, &rows, listExpiredStagedAttachmentsSQL, cutoff, limit); err != nil {
		return nil, fmt.Errorf("mysql.ListExpiredStagedAttachments: %w", err)
	}
	return rows, nil
}

// DeleteStagedAttachment removes one staged row, reporting whether a row was
// actually deleted. The session_id IS NULL guard makes the delete lose the
// race against a concurrent claim: (false, nil) means the row is gone or no
// longer staged, and the caller MUST NOT touch the file.
func (s *Store) DeleteStagedAttachment(ctx context.Context, id string) (bool, error) {
	logQuery(ctx, "attachment.delete_staged", deleteStagedAttachmentSQL, id)
	res, err := s.db.ExecContext(ctx, deleteStagedAttachmentSQL, id)
	if err != nil {
		return false, fmt.Errorf("mysql.DeleteStagedAttachment: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mysql.DeleteStagedAttachment: rows affected: %w", err)
	}
	return affected == 1, nil
}

// RestoreStagedAttachment re-registers an expired staged row after the sweeper
// won the delete race but could not remove the file. The duplicate-key branch
// is a no-op so a concurrently recreated/claimed row is never clobbered.
func (s *Store) RestoreStagedAttachment(ctx context.Context, a model.Attachment) error {
	logQuery(ctx, "attachment.restore_staged", restoreStagedAttachmentSQL, a.ID)
	params := map[string]any{
		"id":          a.ID,
		"user_id":     a.UserID,
		"filename":    a.Filename,
		"mime":        a.MIME,
		"size":        a.Size,
		"message_id":  a.MessageID,
		"create_time": a.CreateTime,
	}
	if _, err := sqlx.NamedExecContext(ctx, s.db, restoreStagedAttachmentSQL, params); err != nil {
		return fmt.Errorf("mysql.RestoreStagedAttachment: %w", err)
	}
	return nil
}
