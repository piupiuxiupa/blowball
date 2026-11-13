package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/pkg/model"
)

// ErrScheduledSessionConflict reports that an idempotent scheduled-session
// creation found an existing row with a different task/occurrence/run identity.
// It is never retried as a transient failure.
var ErrScheduledSessionConflict = errors.New("scheduled session identity conflict")

// CreateSession persists an interactive session row. A session carrying
// scheduled-task identity is rejected: task conversations must use the
// scheduled table group, never an ordinary placeholder row.
func (s *SessionStore) CreateSession(ctx context.Context, sess model.Session) error {
	if s.scheduled() {
		_, err := s.CreateScheduledSession(ctx, sess)
		return err
	}
	if sess.TaskID != "" || sess.OccurrenceID != "" {
		return fmt.Errorf("mysql.CreateSession: interactive session cannot carry task identity")
	}
	query := `INSERT INTO ` + s.tables.Sessions + ` (session_id, user_id, trace_id)
VALUES (:session_id, :user_id, :trace_id)`
	logQuery(ctx, "session.create", query)
	_, err := sqlx.NamedExecContext(ctx, s.db, query, sess)
	return err
}

// CreateScheduledSession idempotently creates the one session belonging to an
// occurrence. The stable session/occurrence/run identity is pre-allocated by
// the Scheduler, so a repeated preparation must return the SAME row. A
// duplicate key with a different task/occurrence/run/owner identity is a hard
// conflict — never a second session and never a silent overwrite.
func (s *SessionStore) CreateScheduledSession(ctx context.Context, sess model.Session) (*model.Session, error) {
	if !s.scheduled() {
		return nil, fmt.Errorf("mysql.CreateScheduledSession: store is not bound to the scheduled source")
	}
	if sess.SessionID == "" || sess.UserID == "" || sess.TaskID == "" || sess.OccurrenceID == "" || sess.RunID == "" {
		return nil, fmt.Errorf("mysql.CreateScheduledSession: session/user/task/occurrence/run identities are required")
	}
	query := `INSERT INTO ` + s.tables.Sessions + `
    (session_id, user_id, task_id, occurrence_id, run_id, trace_id)
VALUES (:session_id, :user_id, :task_id, :occurrence_id, :run_id, :trace_id)
ON DUPLICATE KEY UPDATE session_id = session_id`
	logQuery(ctx, "scheduled_session.create", query)
	if _, err := sqlx.NamedExecContext(ctx, s.db, query, sess); err != nil {
		return nil, err
	}
	existing, err := s.GetScheduledSessionByID(ctx, sess.SessionID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("mysql.CreateScheduledSession: created row is not readable")
	}
	if existing.UserID != sess.UserID || existing.TaskID != sess.TaskID ||
		existing.OccurrenceID != sess.OccurrenceID || existing.RunID != sess.RunID {
		return nil, fmt.Errorf("%w: session %s", ErrScheduledSessionConflict, sess.SessionID)
	}
	return existing, nil
}

// GetSessionByID returns the session matching sessionID from this view's table
// group, or (nil, nil) when no such session exists. There is no cross-table
// fallback: an ID that exists in the other group reads as missing.
func (s *SessionStore) GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error) {
	if s.scheduled() {
		return s.GetScheduledSessionByID(ctx, sessionID)
	}
	query := `SELECT session_id, user_id, trace_id, update_time, create_time, context_compacted
FROM ` + s.tables.Sessions + `
WHERE session_id = ?
LIMIT 1`
	logQuery(ctx, "session.get_by_id", query, sessionID)

	var sess model.Session
	err := s.db.GetContext(ctx, &sess, query, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.HydrateSource(s.source)
	return &sess, nil
}

// GetScheduledSessionByID is the scheduled-group read. It carries the task
// linkage so callers can validate occurrence/run provenance without trusting a
// client-supplied label.
func (s *SessionStore) GetScheduledSessionByID(ctx context.Context, sessionID string) (*model.Session, error) {
	if !s.scheduled() {
		return nil, fmt.Errorf("mysql.GetScheduledSessionByID: store is not bound to the scheduled source")
	}
	query := `SELECT session_id, user_id, task_id, occurrence_id, run_id, trace_id,
       update_time, create_time, context_compacted
FROM ` + s.tables.Sessions + `
WHERE session_id = ?
LIMIT 1`
	logQuery(ctx, "scheduled_session.get_by_id", query, sessionID)

	var sess model.Session
	err := s.db.GetContext(ctx, &sess, query, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.HydrateSource(s.source)
	return &sess, nil
}

// ListSessionsByUser returns every session for the given user ordered by
// update_time DESC.
func (s *SessionStore) ListSessionsByUser(ctx context.Context, userID string) ([]model.Session, error) {
	cols := `session_id, user_id, trace_id, update_time, create_time, context_compacted`
	if s.scheduled() {
		cols = `session_id, user_id, task_id, occurrence_id, run_id, trace_id, update_time, create_time, context_compacted`
	}
	query := `SELECT ` + cols + ` FROM ` + s.tables.Sessions + ` WHERE user_id = ? ORDER BY update_time DESC`
	logQuery(ctx, "session.list_by_user", query, userID)

	var sessions []model.Session
	if err := s.db.SelectContext(ctx, &sessions, query, userID); err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].HydrateSource(s.source)
	}
	return sessions, nil
}

// SessionWithTitle mirrors the joined sessions+titles row projected by
// ListSessionsWithTitle. Title is empty when no titles row exists.
type SessionWithTitle struct {
	SessionID        string    `db:"session_id"        json:"session_id"`
	UserID           string    `db:"user_id"           json:"user_id"`
	TraceID          string    `db:"trace_id"          json:"trace_id"`
	UpdateTime       time.Time `db:"update_time"       json:"update_time"`
	CreateTime       time.Time `db:"create_time"       json:"create_time"`
	ContextCompacted bool      `db:"context_compacted" json:"context_compacted"`
	Title            string    `db:"title"             json:"title"`
	// Scheduled linkage; zero for interactive rows.
	TaskID       string `db:"task_id"       json:"task_id,omitempty"`
	OccurrenceID string `db:"occurrence_id" json:"occurrence_id,omitempty"`
	RunID        string `db:"run_id"        json:"run_id,omitempty"`
}

// ListSessionsWithTitle returns every session owned by userID left-joined onto
// its title row. For the scheduled view the result additionally carries task
// linkage; the ordinary view leaves those columns empty.
func (s *SessionStore) ListSessionsWithTitle(ctx context.Context, userID string) ([]SessionWithTitle, error) {
	var query string
	if s.scheduled() {
		query = `SELECT s.session_id AS session_id, s.user_id AS user_id, s.task_id AS task_id,
       s.occurrence_id AS occurrence_id, s.run_id AS run_id, s.trace_id AS trace_id,
       s.update_time AS update_time, s.create_time AS create_time,
       s.context_compacted AS context_compacted, COALESCE(t.title, '') AS title
FROM ` + s.tables.Sessions + ` s
LEFT JOIN ` + s.tables.Titles + ` t ON t.session_id = s.session_id
WHERE s.user_id = ?
ORDER BY s.update_time DESC`
	} else {
		query = `SELECT s.session_id AS session_id, s.user_id AS user_id, s.trace_id AS trace_id,
       s.update_time AS update_time, s.create_time AS create_time,
       s.context_compacted AS context_compacted,
       COALESCE(t.title, '') AS title
FROM ` + s.tables.Sessions + ` s
LEFT JOIN ` + s.tables.Titles + ` t ON t.session_id = s.session_id
WHERE s.user_id = ?
ORDER BY s.update_time DESC`
	}
	logQuery(ctx, "session.list_with_title", query, userID)

	var rows []SessionWithTitle
	if err := s.db.SelectContext(ctx, &rows, query, userID); err != nil {
		return nil, err
	}
	return rows, nil
}

// ListScheduledSessions returns one owner-scoped page of scheduled sessions,
// optionally filtered by task, newest first. limit is clamped to [1, 100];
// offset pagination is used because the list contract has no message-order
// cursor requirement.
func (s *SessionStore) ListScheduledSessions(ctx context.Context, userID, taskID string, offset, limit int) ([]SessionWithTitle, error) {
	if !s.scheduled() {
		return nil, fmt.Errorf("mysql.ListScheduledSessions: store is not bound to the scheduled source")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT s.session_id AS session_id, s.user_id AS user_id, s.task_id AS task_id,
       s.occurrence_id AS occurrence_id, s.run_id AS run_id, s.trace_id AS trace_id,
       s.update_time AS update_time, s.create_time AS create_time,
       s.context_compacted AS context_compacted, COALESCE(t.title, '') AS title
FROM ` + s.tables.Sessions + ` s
LEFT JOIN ` + s.tables.Titles + ` t ON t.session_id = s.session_id
WHERE s.user_id = ? AND (? = '' OR s.task_id = ?)
ORDER BY s.update_time DESC, s.session_id DESC
LIMIT ? OFFSET ?`
	logQuery(ctx, "scheduled_session.list", query, userID, taskID, taskID, limit, offset)

	var rows []SessionWithTitle
	if err := s.db.SelectContext(ctx, &rows, query, userID, taskID, taskID, limit, offset); err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateSessionTime touches the source's session row so it bubbles to the top
// of the list.
func (s *SessionStore) UpdateSessionTime(ctx context.Context, sessionID string) error {
	query := `UPDATE ` + s.tables.Sessions + ` SET update_time = NOW() WHERE session_id = ?`
	logQuery(ctx, "session.update_time", query, sessionID)
	_, err := s.db.ExecContext(ctx, query, sessionID)
	return err
}

// DeleteSession archives the session's conversation rows into the mirror
// tables and purges the live row in one transaction. The cascade removes child
// rows; the archive captures the conversation-bearing ones.
func (s *SessionStore) DeleteSession(ctx context.Context, sessionID string) error {
	if s.scheduled() {
		return s.deleteScheduledSession(ctx, sessionID)
	}
	deletionID := newDeletionID()
	deletedAt := time.Now().UTC()
	logQuery(ctx, "session.delete_archive", "archive "+s.tables.Sessions, deletionID, deletedAt, sessionID)

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql.DeleteSession: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	archive := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO ` + s.tables.SessionsDeleted + ` (session_id, user_id, trace_id, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, user_id, trace_id, update_time, create_time, ?, ? FROM ` + s.tables.Sessions + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.TitlesDeleted + ` (session_id, title, trace_id, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, title, trace_id, update_time, create_time, ?, ? FROM ` + s.tables.Titles + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.MessagesDeleted + ` (id, session_id, msg_time, agent, msg_index, role, event_type, content, attachments_json, trace_id, update_time, deletion_id, deleted_at)
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, attachments_json, trace_id, update_time, ?, ? FROM ` + s.tables.Messages + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.AttachmentsDeleted + ` (id, user_id, session_id, filename, mime, size, message_id, create_time, deleted_at, deletion_id)
SELECT id, user_id, session_id, filename, mime, size, message_id, create_time, ?, ? FROM ` + s.tables.Attachments + ` WHERE session_id = ?`,
			[]any{deletedAt, deletionID, sessionID}},
	}
	for _, a := range archive {
		if _, err := tx.ExecContext(ctx, a.query, a.args...); err != nil {
			return fmt.Errorf("mysql.DeleteSession: archive: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.tables.Sessions+` WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("mysql.DeleteSession: purge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql.DeleteSession: commit: %w", err)
	}
	return nil
}

// deleteScheduledSession archives the scheduled conversation group. The
// scheduled session mirror carries the task/occurrence/run provenance so a
// deleted occurrence remains attributable.
func (s *SessionStore) deleteScheduledSession(ctx context.Context, sessionID string) error {
	deletionID := newDeletionID()
	deletedAt := time.Now().UTC()
	logQuery(ctx, "scheduled_session.delete_archive", "archive "+s.tables.Sessions, deletionID, deletedAt, sessionID)

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql.DeleteScheduledSession: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	archive := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO ` + s.tables.SessionsDeleted + ` (session_id, user_id, task_id, occurrence_id, run_id, trace_id, context_compacted, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, user_id, task_id, occurrence_id, run_id, trace_id, context_compacted, update_time, create_time, ?, ? FROM ` + s.tables.Sessions + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.TitlesDeleted + ` (session_id, title, trace_id, update_time, create_time, deletion_id, deleted_at)
SELECT session_id, title, trace_id, update_time, create_time, ?, ? FROM ` + s.tables.Titles + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.MessagesDeleted + ` (id, session_id, msg_time, agent, msg_index, role, event_type, content, attachments_json, trace_id, update_time, deletion_id, deleted_at)
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, attachments_json, trace_id, update_time, ?, ? FROM ` + s.tables.Messages + ` WHERE session_id = ?`,
			[]any{deletionID, deletedAt, sessionID}},
		{`INSERT INTO ` + s.tables.AttachmentsDeleted + ` (id, user_id, session_id, filename, mime, size, message_id, create_time, deleted_at, deletion_id)
SELECT id, user_id, session_id, filename, mime, size, message_id, create_time, ?, ? FROM ` + s.tables.Attachments + ` WHERE session_id = ?`,
			[]any{deletedAt, deletionID, sessionID}},
	}
	for _, a := range archive {
		if _, err := tx.ExecContext(ctx, a.query, a.args...); err != nil {
			return fmt.Errorf("mysql.DeleteScheduledSession: archive: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.tables.Sessions+` WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("mysql.DeleteScheduledSession: purge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql.DeleteScheduledSession: commit: %w", err)
	}
	return nil
}

// SessionContentExists checks whether a session row still exists in the table
// group named by its explicit source. It is used only to annotate historical
// occurrences with "content deleted"; it never infers a source by fallback.
func (s *Store) SessionContentExists(ctx context.Context, source model.SessionSource, sessionID string) (bool, error) {
	if !source.Valid() {
		return false, fmt.Errorf("mysql.SessionContentExists: invalid source %q", source)
	}
	sess, err := s.ForSource(source).GetSessionByID(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sess != nil, nil
}

// ── interactive-source compatibility wrappers ──────────────────────────────
//
// The raw *Store keeps its pre-split method surface bound to the interactive
// table group. Production scheduling code receives an explicit ForSource view,
// so these wrappers can never serve a scheduled write.

// CreateSession persists an interactive session.
func (s *Store) CreateSession(ctx context.Context, sess model.Session) error {
	return s.ForSource(model.SessionSourceInteractive).CreateSession(ctx, sess)
}

// GetSessionByID reads an interactive session.
func (s *Store) GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error) {
	return s.ForSource(model.SessionSourceInteractive).GetSessionByID(ctx, sessionID)
}

// ListSessionsByUser lists an interactive user's sessions.
func (s *Store) ListSessionsByUser(ctx context.Context, userID string) ([]model.Session, error) {
	return s.ForSource(model.SessionSourceInteractive).ListSessionsByUser(ctx, userID)
}

// ListSessionsWithTitle lists an interactive user's sessions with titles.
func (s *Store) ListSessionsWithTitle(ctx context.Context, userID string) ([]SessionWithTitle, error) {
	return s.ForSource(model.SessionSourceInteractive).ListSessionsWithTitle(ctx, userID)
}

// UpdateSessionTime touches an interactive session.
func (s *Store) UpdateSessionTime(ctx context.Context, sessionID string) error {
	return s.ForSource(model.SessionSourceInteractive).UpdateSessionTime(ctx, sessionID)
}

// DeleteSession archives and purges an interactive session.
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	return s.ForSource(model.SessionSourceInteractive).DeleteSession(ctx, sessionID)
}
