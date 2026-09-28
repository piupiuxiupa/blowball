package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// insertToolCallsSQL inserts tool-interaction rows in one multi-value
// statement. INSERT IGNORE plus uk_tool_calls_session_call collapses
// redelivery (round-batch retry) to one row per call.
const insertToolCallsSQL = `
INSERT IGNORE INTO tool_calls (session_id, turn_id, round_no, tool_call_id, name, arguments, result, status)
VALUES %s
`

// listToolCallsSQL returns every tool interaction for sessionID in insertion
// order (which follows (turn, round) by construction — round batches are
// written in order). result COALESCE keeps the plain model field.
const listToolCallsSQL = `
SELECT id, session_id, turn_id, round_no, tool_call_id, name, arguments,
       COALESCE(result, '') AS result, status, create_time, update_time
FROM tool_calls
WHERE session_id = ?
ORDER BY id ASC
`

// AppendToolCalls inserts calls idempotently (see insertToolCallsSQL).
func (s *Store) AppendToolCalls(ctx context.Context, calls []model.ToolCall) error {
	if len(calls) == 0 {
		return nil
	}
	return insertToolCalls(ctx, s.db, calls)
}

// ListToolCalls returns every tool interaction for sessionID in (turn, round)
// order, or an empty slice when the session has none.
func (s *Store) ListToolCalls(ctx context.Context, sessionID string) ([]model.ToolCall, error) {
	logQuery(ctx, "tool_calls.list", listToolCallsSQL, sessionID)
	var calls []model.ToolCall
	if err := s.db.SelectContext(ctx, &calls, listToolCallsSQL, sessionID); err != nil {
		return nil, err
	}
	return calls, nil
}

// AppendFinalBatch writes one round batch — final message rows and their tool
// calls — in a single transaction, so a round is all-or-nothing durable
// (refactor-message-storage: cross-table round persistence).
func (s *Store) AppendFinalBatch(ctx context.Context, msgs []model.Message, calls []model.ToolCall) error {
	if len(msgs) == 0 && len(calls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append final batch: begin: %w", err)
	}
	defer tx.Rollback()

	if len(msgs) > 0 {
		if _, err := appendMessages(ctx, tx, msgs); err != nil {
			return fmt.Errorf("append final batch: messages: %w", err)
		}
	}
	if len(calls) > 0 {
		if err := insertToolCalls(ctx, tx, calls); err != nil {
			return fmt.Errorf("append final batch: tool_calls: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append final batch: commit: %w", err)
	}
	return nil
}

// insertToolCalls expands insertToolCallsSQL over calls and executes it on q
// (the store or an open transaction).
func insertToolCalls(ctx context.Context, q sqlx.ExtContext, calls []model.ToolCall) error {
	placeholders := make([]string, 0, len(calls))
	args := make([]any, 0, len(calls)*8)
	for _, c := range calls {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			c.SessionID,
			c.TurnID,
			c.RoundNo,
			c.ToolCallID,
			c.Name,
			c.Arguments,
			nilIfEmpty(c.Result),
			c.Status,
		)
	}
	query := fmt.Sprintf(insertToolCallsSQL, strings.Join(placeholders, ", "))
	logQuery(ctx, "tool_calls.append_batch", query)
	_, err := q.ExecContext(ctx, query, args...)
	return err
}
