package mysql

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// archiveSessionsSQL copies the doomed session row verbatim into
// sessions_deleted, stamping it with the operation's deletion_id and deleted_at.
// Placeholders are (deletion_id, deleted_at, session_id); deleted_at is bound
// in Go (not server-side NOW()) so every mirror table in the same delete
// receives an identical timestamp even across a wall-clock second boundary —
// NOW() is per-statement, which would let sessions/titles/messages diverge.
const archiveSessionsSQL = `
INSERT INTO sessions_deleted (session_id, user_id, trace_id, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, user_id, trace_id, update_time, create_time, ?, ?
FROM sessions
WHERE session_id = ?
`

// archiveTitlesSQL copies every title row belonging to sessionID into
// titles_deleted. Placeholders are (deletion_id, deleted_at, session_id).
const archiveTitlesSQL = `
INSERT INTO titles_deleted (session_id, title, trace_id, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, title, trace_id, update_time, create_time, ?, ?
FROM titles
WHERE session_id = ?
`

// archiveMessagesSQL copies every message row belonging to sessionID into
// messages_deleted, preserving the original id as a plain BIGINT primary key.
// The MEDIUMTEXT content is copied server-side by INSERT...SELECT so it never
// enters Go memory, keeping memory pressure bounded regardless of how large a
// session's history is. Placeholders are (deletion_id, deleted_at, session_id).
const archiveMessagesSQL = `
INSERT INTO messages_deleted (id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time, deletion_id, deleted_at)
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time, ?, ?
FROM messages
WHERE session_id = ?
`

// deleteSessionSQL removes the session row. titles and messages are purged
// automatically by their ON DELETE CASCADE foreign keys, so a single delete is
// enough to clear every live row for the session.
const deleteSessionSQL = `
DELETE FROM sessions
WHERE session_id = ?
`

// DeleteSession archives a session and all of its titles/messages into the
// *_deleted mirror tables and then purges the live rows, all inside a single
// transaction so the archive-and-purge is atomic: any failure rolls back and
// leaves the live data intact with no partial archive.
//
// The same deletion_id (a freshly minted UUID) AND the same deleted_at (a Go
// timestamp captured once per call) are stamped on every mirrored row, so the
// rows from one delete share identical audit values per the deletion-archive
// spec's "same deletion_id and same deleted_at" requirement.
//
// If the session does not exist the INSERT...SELECT statements copy zero rows,
// the DELETE affects zero rows, and the transaction commits returning nil — the
// call is idempotent, matching the store-wide (nil, nil)-on-not-found convention.
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	deletionID := uuid.NewString()
	deletedAt := time.Now().UTC()
	logQuery(ctx, "session.delete_archive", archiveSessionsSQL, deletionID, deletedAt, sessionID)

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql.DeleteSession: begin tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op after Commit; on the error paths below it rolls
		// the in-flight transaction back so nothing is partially archived.
		_ = tx.Rollback()
	}()

	for _, q := range []string{archiveSessionsSQL, archiveTitlesSQL, archiveMessagesSQL} {
		if _, err := tx.ExecContext(ctx, q, deletionID, deletedAt, sessionID); err != nil {
			return fmt.Errorf("mysql.DeleteSession: archive: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, deleteSessionSQL, sessionID); err != nil {
		return fmt.Errorf("mysql.DeleteSession: purge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql.DeleteSession: commit: %w", err)
	}
	return nil
}
