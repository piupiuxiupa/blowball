package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// createSessionSQL inserts a new session row. session_id is caller-supplied
// (a UUID minted upstream) so the session record is immediately addressable.
const createSessionSQL = `
INSERT INTO sessions (session_id, user_id, trace_id)
VALUES (:session_id, :user_id, :trace_id)
`

// listSessionsByUserSQL returns every session owned by userID, most-recently
// updated first. The index idx_sessions_user_update backs this query.
const listSessionsByUserSQL = `
SELECT session_id, user_id, trace_id, update_time, create_time
FROM sessions
WHERE user_id = ?
ORDER BY update_time DESC
`

// CreateSession persists s into the sessions table.
func (s *Store) CreateSession(ctx context.Context, sess model.Session) error {
	logQuery(ctx, "session.create", createSessionSQL)
	_, err := sqlx.NamedExecContext(ctx, s.db, createSessionSQL, sess)
	if err != nil {
		return err
	}
	return nil
}

// ListSessionsByUser returns all sessions for the given user ordered by
// update_time DESC. An empty (non-nil) slice is returned when the user has no
// sessions.
func (s *Store) ListSessionsByUser(ctx context.Context, userID string) ([]model.Session, error) {
	logQuery(ctx, "session.list_by_user", listSessionsByUserSQL, userID)

	var sessions []model.Session
	if err := s.db.SelectContext(ctx, &sessions, listSessionsByUserSQL, userID); err != nil {
		return nil, err
	}
	return sessions, nil
}

// getSessionByIDSQL returns the row for a single session_id PK lookup. Used by
// the service layer to decide whether EnsureSession needs to create the row.
const getSessionByIDSQL = `
SELECT session_id, user_id, trace_id, update_time, create_time
FROM sessions
WHERE session_id = ?
LIMIT 1
`

// GetSessionByID returns the session matching sessionID, or (nil, nil) when no
// such session exists.
func (s *Store) GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error) {
	logQuery(ctx, "session.get_by_id", getSessionByIDSQL, sessionID)

	var sess model.Session
	err := s.db.GetContext(ctx, &sess, getSessionByIDSQL, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// listSessionsWithTitleSQL returns every session owned by userID joined to the
// titles table via LEFT JOIN so sessions without a generated title still appear
// (with an empty title). The result is ordered by sessions.update_time DESC,
// matching the session-management spec's "List sessions" scenario.
const listSessionsWithTitleSQL = `
SELECT s.session_id AS session_id, s.user_id AS user_id, s.trace_id AS trace_id,
       s.update_time AS update_time, s.create_time AS create_time,
       COALESCE(t.title, '') AS title
FROM sessions s
LEFT JOIN titles t ON t.session_id = s.session_id
WHERE s.user_id = ?
ORDER BY s.update_time DESC
`

// SessionWithTitle mirrors the joined sessions+titles row projected by
// ListSessionsWithTitle. Title is empty when no titles row exists for the
// session. It is intended for read-only consumption by the session service.
type SessionWithTitle struct {
	SessionID  string    `db:"session_id"  json:"session_id"`
	UserID     string    `db:"user_id"     json:"user_id"`
	TraceID    string    `db:"trace_id"    json:"trace_id"`
	UpdateTime time.Time `db:"update_time" json:"update_time"`
	CreateTime time.Time `db:"create_time" json:"create_time"`
	Title      string    `db:"title"       json:"title"`
}

// ListSessionsWithTitle returns every session owned by userID left-joined onto
// its title row. Sessions with no title row carry an empty Title. The slice is
// ordered by sessions.update_time DESC. An empty (non-nil) slice is returned
// when the user has no sessions.
func (s *Store) ListSessionsWithTitle(ctx context.Context, userID string) ([]SessionWithTitle, error) {
	logQuery(ctx, "session.list_with_title", listSessionsWithTitleSQL, userID)

	var rows []SessionWithTitle
	if err := s.db.SelectContext(ctx, &rows, listSessionsWithTitleSQL, userID); err != nil {
		return nil, err
	}
	return rows, nil
}
