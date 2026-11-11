package mysql

import (
	"context"

	"github.com/lush/blowball/internal/model"
)

// SaveTurnUsage inserts one turn cost row into this view's usage table.
func (s *SessionStore) SaveTurnUsage(ctx context.Context, tu model.TurnUsage) error {
	query := `INSERT INTO ` + s.tables.TurnUsage + ` (session_id, trace_id, user_id, usage_json, total_tokens, model, context_tokens)
VALUES (:session_id, :trace_id, :user_id, :usage_json, :total_tokens, NULLIF(:model, ''), :context_tokens)`
	logQuery(ctx, "turn_usage.save", query)
	_, err := s.db.NamedExecContext(ctx, query, tu)
	return err
}

// SaveTurnUsage writes the interactive turn_usage table.
func (s *Store) SaveTurnUsage(ctx context.Context, tu model.TurnUsage) error {
	return s.ForSource(model.SessionSourceInteractive).SaveTurnUsage(ctx, tu)
}
