package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/pkg/model"
)

func insertToolCallsSQL(t Tables) string {
	return `INSERT IGNORE INTO ` + t.ToolCalls + ` (session_id, turn_id, tool_call_id, name, arguments, result, status)
VALUES %s`
}

func listToolCallsSQL(t Tables) string {
	return `SELECT id, session_id, turn_id, tool_call_id, name, arguments,
       COALESCE(result, '') AS result, status, create_time, update_time
FROM ` + t.ToolCalls + `
WHERE session_id = ?
ORDER BY id ASC`
}

// AppendToolCalls inserts calls idempotently.
func (s *SessionStore) AppendToolCalls(ctx context.Context, calls []model.ToolCall) error {
	if len(calls) == 0 {
		return nil
	}
	return insertToolCalls(ctx, s.db, s.tables, calls)
}

// ListToolCalls returns every tool interaction for sessionID in insertion
// order, or an empty slice when the session has none.
func (s *SessionStore) ListToolCalls(ctx context.Context, sessionID string) ([]model.ToolCall, error) {
	query := listToolCallsSQL(s.tables)
	logQuery(ctx, "tool_calls.list", query, sessionID)
	var calls []model.ToolCall
	if err := s.db.SelectContext(ctx, &calls, query, sessionID); err != nil {
		return nil, err
	}
	return calls, nil
}

// AppendFinalBatch writes one round batch — final message rows and their tool
// calls — in a single transaction, so a round is all-or-nothing durable.
func (s *SessionStore) AppendFinalBatch(ctx context.Context, msgs []model.Message, calls []model.ToolCall) error {
	if len(msgs) == 0 && len(calls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append final batch: begin: %w", err)
	}
	defer tx.Rollback()

	if len(msgs) > 0 {
		if _, err := appendMessages(ctx, tx, s.tables, msgs); err != nil {
			return fmt.Errorf("append final batch: messages: %w", err)
		}
	}
	if len(calls) > 0 {
		if err := insertToolCalls(ctx, tx, s.tables, calls); err != nil {
			return fmt.Errorf("append final batch: tool_calls: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append final batch: commit: %w", err)
	}
	return nil
}

// insertToolCalls expands the batch insert over calls and executes it on q
// (the store or an open transaction).
func insertToolCalls(ctx context.Context, q sqlx.ExtContext, tables Tables, calls []model.ToolCall) error {
	placeholders := make([]string, 0, len(calls))
	args := make([]any, 0, len(calls)*7)
	for _, c := range calls {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			c.SessionID,
			c.TurnID,
			c.ToolCallID,
			c.Name,
			c.Arguments,
			nilIfEmpty(c.Result),
			c.Status,
		)
	}
	query := fmt.Sprintf(insertToolCallsSQL(tables), strings.Join(placeholders, ", "))
	logQuery(ctx, "tool_calls.append_batch", query)
	_, err := q.ExecContext(ctx, query, args...)
	return err
}

// ── interactive-source compatibility wrappers ──────────────────────────────

// AppendToolCalls inserts into the interactive tool_calls table.
func (s *Store) AppendToolCalls(ctx context.Context, calls []model.ToolCall) error {
	return s.ForSource(model.SessionSourceInteractive).AppendToolCalls(ctx, calls)
}

// ListToolCalls reads the interactive tool_calls table.
func (s *Store) ListToolCalls(ctx context.Context, sessionID string) ([]model.ToolCall, error) {
	return s.ForSource(model.SessionSourceInteractive).ListToolCalls(ctx, sessionID)
}

// AppendFinalBatch writes an interactive round batch.
func (s *Store) AppendFinalBatch(ctx context.Context, msgs []model.Message, calls []model.ToolCall) error {
	return s.ForSource(model.SessionSourceInteractive).AppendFinalBatch(ctx, msgs, calls)
}
