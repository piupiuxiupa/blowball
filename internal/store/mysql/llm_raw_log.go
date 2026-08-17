package mysql

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// appendRawLogsSQL inserts multiple llm_raw_log rows in a single statement.
// The VALUES clause is expanded at runtime by AppendRawLogs. update_time is
// omitted so the column's DEFAULT CURRENT_TIMESTAMP(3) stamps the row.
const appendRawLogsSQL = `
INSERT INTO llm_raw_log (call_id, seq, frame_index, trace_id, session_id, user_id, agent, kind, model, finish_reason, http_status, duration_ms, raw, msg_time)
VALUES %s
`

// AppendRawLogs inserts logs into llm_raw_log. This is a write-only
// observability path (the llm-raw-capture spec): a single bad row — most
// plausibly a raw payload exceeding MEDIUMTEXT's 16MB ceiling — MUST NOT take
// the rest of its flush batch down. So the multi-value INSERT is attempted
// first, and on any batch-level failure the rows are retried one-by-one with
// the failing row(s) dropped and logged. The returned error therefore only
// fires when even the per-row fallback could not run (e.g. the connection is
// gone); callers treat it as best-effort.
func (s *Store) AppendRawLogs(ctx context.Context, logs []model.LLMRawLog) error {
	if len(logs) == 0 {
		return nil
	}

	placeholders := make([]string, 0, len(logs))
	args := make([]any, 0, len(logs)*14)
	for _, l := range logs {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			l.CallID,
			l.Seq,
			l.FrameIndex,
			l.TraceID,
			l.SessionID,
			l.UserID,
			l.Agent,
			l.Kind,
			l.Model,
			l.FinishReason,
			l.HTTPStatus,
			l.DurationMS,
			l.Raw,
			l.MsgTime,
		)
	}

	query := fmt.Sprintf(appendRawLogsSQL, strings.Join(placeholders, ", "))
	logQuery(ctx, "llm_raw_log.append_batch", query)

	_, batchErr := s.db.ExecContext(ctx, query, args...)
	if batchErr == nil {
		return nil
	}
	logger.L().Warn("llm_raw_log batch insert failed; retrying rows individually",
		zap.Int("rows", len(logs)),
		zap.Error(batchErr))

	// Per-row fallback: isolate failures to the offending row.
	for _, l := range logs {
		if _, err := s.db.ExecContext(ctx, queryOneRawLogSQL, rowArgs(l)...); err != nil {
			logger.L().Error("llm_raw_log row insert failed; dropping row",
				zap.String("call_id", l.CallID),
				zap.String("kind", l.Kind),
				zap.Int("seq", l.Seq),
				zap.Int("raw_bytes", len(l.Raw)),
				zap.Error(err))
		}
	}
	return nil
}

// queryOneRawLogSQL is the single-row form of appendRawLogsSQL used by the
// per-row fallback path.
const queryOneRawLogSQL = `
INSERT INTO llm_raw_log (call_id, seq, frame_index, trace_id, session_id, user_id, agent, kind, model, finish_reason, http_status, duration_ms, raw, msg_time)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

// rowArgs returns the positional args for one LLMRawLog row in
// queryOneRawLogSQL column order.
func rowArgs(l model.LLMRawLog) []any {
	return []any{
		l.CallID,
		l.Seq,
		l.FrameIndex,
		l.TraceID,
		l.SessionID,
		l.UserID,
		l.Agent,
		l.Kind,
		l.Model,
		l.FinishReason,
		l.HTTPStatus,
		l.DurationMS,
		l.Raw,
		l.MsgTime,
	}
}
