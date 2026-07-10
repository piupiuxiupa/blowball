package mysql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// upsertTitleSQL inserts a new title row, or replaces the title and trace_id
// when a row already exists for the same session_id. The MySQL-specific
// ON DUPLICATE KEY UPDATE keeps it a single round trip and avoids a separate
// "does it exist?" probe. is_manual is always FALSE for AI-generated titles.
const upsertTitleSQL = `
INSERT INTO titles (session_id, title, trace_id, is_manual)
VALUES (:session_id, :title, :trace_id, FALSE)
ON DUPLICATE KEY UPDATE
    title      = VALUES(title),
    trace_id   = VALUES(trace_id),
    is_manual  = FALSE
`

// upsertTitleManualSQL is used for user-edited titles. It inserts a new row or
// updates an existing one while preserving is_manual = TRUE so subsequent AI
// generation skips it.
const upsertTitleManualSQL = `
INSERT INTO titles (session_id, title, trace_id, is_manual)
VALUES (:session_id, :title, :trace_id, TRUE)
ON DUPLICATE KEY UPDATE
    title      = VALUES(title),
    trace_id   = VALUES(trace_id),
    is_manual  = TRUE
`

// getTitleSQL returns the title row for sessionID.
const getTitleSQL = `
SELECT session_id, title, trace_id, is_manual, update_time, create_time
FROM titles
WHERE session_id = ?
LIMIT 1
`

// UpsertTitle creates or replaces the title for t.SessionID.
func (s *Store) UpsertTitle(ctx context.Context, t model.Title) error {
	logQuery(ctx, "title.upsert", upsertTitleSQL)
	_, err := sqlx.NamedExecContext(ctx, s.db, upsertTitleSQL, t)
	if err != nil {
		return err
	}
	return nil
}

// UpsertTitleManual persists a user-edited title with is_manual = TRUE.
func (s *Store) UpsertTitleManual(ctx context.Context, t model.Title) error {
	logQuery(ctx, "title.upsert_manual", upsertTitleManualSQL)
	_, err := sqlx.NamedExecContext(ctx, s.db, upsertTitleManualSQL, t)
	if err != nil {
		return err
	}
	return nil
}

// GetTitle returns the title associated with sessionID, or (nil, nil) when no
// title has been generated yet.
func (s *Store) GetTitle(ctx context.Context, sessionID string) (*model.Title, error) {
	logQuery(ctx, "title.get", getTitleSQL, sessionID)

	var t model.Title
	err := s.db.GetContext(ctx, &t, getTitleSQL, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
