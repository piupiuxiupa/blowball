package mysql

import (
	"context"

	"github.com/lush/blowball/internal/model"
)

// saveTurnUsageSQL inserts one turn_usage row recording a completed chat turn's
// per-agent cost. id is AUTO_INCREMENT. The redundant total_tokens column lets
// per-session cost be summed without parsing usage_json.
const saveTurnUsageSQL = `
INSERT INTO turn_usage (session_id, trace_id, user_id, usage_json, total_tokens)
VALUES (:session_id, :trace_id, :user_id, :usage_json, :total_tokens)
`

// SaveTurnUsage inserts one turn_usage row. It is a write-only path (the
// not-found-returns-(nil,nil) convention does not apply). Callers persist it
// alongside the turn's message batch: usage write failure is logged but must
// NOT roll back the messages (usage is observability data, messages are
// business data — see the turn-cost-tracking spec's "Usage write failure does
// not roll back messages" scenario), so the caller wraps any error handling.
func (s *Store) SaveTurnUsage(ctx context.Context, tu model.TurnUsage) error {
	logQuery(ctx, "turn_usage.save", saveTurnUsageSQL)

	if _, err := s.db.NamedExecContext(ctx, saveTurnUsageSQL, tu); err != nil {
		return err
	}
	return nil
}
