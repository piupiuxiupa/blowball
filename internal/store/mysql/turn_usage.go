package mysql

import (
	"context"

	"github.com/lush/blowball/internal/model"
)

// saveTurnUsageSQL inserts one turn_usage row recording a completed chat turn's
// per-agent cost. id is AUTO_INCREMENT. The redundant total_tokens column lets
// per-session cost be summed without parsing usage_json; model records the
// turn's resolved model name for per-model aggregation (migration 015,
// per-request-model-selection — NULLIF keeps empty Go strings identical to
// pre-migration NULL rows); context_tokens records the LAST round's
// prompt+completion — the authoritative end-of-turn context size the
// turn-start preventive compaction check reads (see migration 013 /
// internal/service/compaction.go).
const saveTurnUsageSQL = `
INSERT INTO turn_usage (session_id, trace_id, user_id, usage_json, total_tokens, model, context_tokens)
VALUES (:session_id, :trace_id, :user_id, :usage_json, :total_tokens, NULLIF(:model, ''), :context_tokens)
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
