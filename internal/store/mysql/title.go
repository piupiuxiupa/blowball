package mysql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

func upsertTitleSQL(t Tables) string {
	return `INSERT INTO ` + t.Titles + ` (session_id, title, trace_id, is_manual)
VALUES (:session_id, :title, :trace_id, FALSE)
ON DUPLICATE KEY UPDATE
    title      = IF(is_manual, title, VALUES(title)),
    trace_id   = IF(is_manual, trace_id, VALUES(trace_id)),
    is_manual  = is_manual`
}

func upsertTitleManualSQL(t Tables) string {
	return `INSERT INTO ` + t.Titles + ` (session_id, title, trace_id, is_manual)
VALUES (:session_id, :title, :trace_id, TRUE)
ON DUPLICATE KEY UPDATE
    title      = VALUES(title),
    trace_id   = VALUES(trace_id),
    is_manual  = TRUE`
}

func getTitleSQL(t Tables) string {
	return `SELECT session_id, title, trace_id, is_manual, update_time, create_time
FROM ` + t.Titles + `
WHERE session_id = ?
LIMIT 1`
}

// UpsertTitle creates or replaces the title for t.SessionID. A pre-existing
// manual title row is left untouched (see the SQL guard).
func (s *SessionStore) UpsertTitle(ctx context.Context, t model.Title) error {
	query := upsertTitleSQL(s.tables)
	logQuery(ctx, "title.upsert", query)
	_, err := sqlx.NamedExecContext(ctx, s.db, query, t)
	return err
}

// UpsertTitleManual persists a user-edited title with is_manual = TRUE.
func (s *SessionStore) UpsertTitleManual(ctx context.Context, t model.Title) error {
	query := upsertTitleManualSQL(s.tables)
	logQuery(ctx, "title.upsert_manual", query)
	_, err := sqlx.NamedExecContext(ctx, s.db, query, t)
	return err
}

// GetTitle returns the title associated with sessionID, or (nil, nil) when no
// title has been generated yet.
func (s *SessionStore) GetTitle(ctx context.Context, sessionID string) (*model.Title, error) {
	query := getTitleSQL(s.tables)
	logQuery(ctx, "title.get", query, sessionID)

	var t model.Title
	err := s.db.GetContext(ctx, &t, query, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ── interactive-source compatibility wrappers ──────────────────────────────

// UpsertTitle writes the interactive titles table.
func (s *Store) UpsertTitle(ctx context.Context, t model.Title) error {
	return s.ForSource(model.SessionSourceInteractive).UpsertTitle(ctx, t)
}

// UpsertTitleManual writes a manual interactive title.
func (s *Store) UpsertTitleManual(ctx context.Context, t model.Title) error {
	return s.ForSource(model.SessionSourceInteractive).UpsertTitleManual(ctx, t)
}

// GetTitle reads the interactive titles table.
func (s *Store) GetTitle(ctx context.Context, sessionID string) (*model.Title, error) {
	return s.ForSource(model.SessionSourceInteractive).GetTitle(ctx, sessionID)
}
