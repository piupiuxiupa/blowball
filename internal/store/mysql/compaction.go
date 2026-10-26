package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lush/blowball/internal/model"
)

// insertCompactionSQL appends one compaction record. The table is append-only
// with latest-wins stitching: the record's id participates in the
// (session_id, id) ordering that LatestCompaction reads back.
const insertCompactionSQL = `
INSERT INTO context_compactions (
    session_id, user_id, trace_id, trigger_kind, trigger_tokens, content,
    boundary_msg_time, boundary_msg_index, boundary_msg_id,
    shadowed_tokens, summary_model, summary_prompt_tokens, summary_completion_tokens,
    retained_results_json, measurement_basis, pre_compaction_tokens, input_budget_tokens,
    post_compaction_tokens, retained_tail_messages, summary_strategy,
    tool_summary_count, tool_activity_count
) VALUES (
    :session_id, :user_id, :trace_id, :trigger_kind, :trigger_tokens, :content,
    :boundary_msg_time, :boundary_msg_index, :boundary_msg_id,
    :shadowed_tokens, :summary_model, :summary_prompt_tokens, :summary_completion_tokens,
    NULLIF(:retained_results_json, ''), NULLIF(:measurement_basis, ''),
    NULLIF(:pre_compaction_tokens, 0), NULLIF(:input_budget_tokens, 0),
    NULLIF(:post_compaction_tokens, 0), NULLIF(:retained_tail_messages, 0),
    NULLIF(:summary_strategy, ''), NULLIF(:tool_summary_count, 0),
    NULLIF(:tool_activity_count, 0)
)
`

// InsertCompaction persists one completed compaction record. It is a
// write-only path (the not-found-returns-(nil,nil) convention does not apply).
// Callers order their writes record-first, then Redis, then the session flag
// (see the context-compaction spec's crash-consistency write order).
func (s *Store) InsertCompaction(ctx context.Context, rec model.ContextCompaction) error {
	logQuery(ctx, "compaction.insert", insertCompactionSQL)
	if _, err := s.db.NamedExecContext(ctx, insertCompactionSQL, rec); err != nil {
		return err
	}
	return nil
}

// latestCompactionSQL returns the newest compaction record for a session. The
// AUTO_INCREMENT id is a strict insert-order proxy, so ORDER BY id DESC LIMIT 1
// is the latest-wins stitch source.
const latestCompactionSQL = `
SELECT id, session_id, user_id, trace_id, trigger_kind, trigger_tokens, content,
       boundary_msg_time, boundary_msg_index, boundary_msg_id,
       shadowed_tokens, summary_model, summary_prompt_tokens, summary_completion_tokens,
       retained_results_json, measurement_basis, pre_compaction_tokens, input_budget_tokens,
       post_compaction_tokens, retained_tail_messages, summary_strategy,
       tool_summary_count, tool_activity_count,
       create_time
FROM context_compactions
WHERE session_id = ?
ORDER BY id DESC
LIMIT 1
`

// compactionRecordRow keeps every post-013 observation column nullable at the
// scan boundary. Existing records predate migration 023, and NULLIF writes use
// NULL for absent observations; scanning those directly into Go strings/ints
// would make the latest authoritative record unreadable.
type compactionRecordRow struct {
	ID                      int64          `db:"id"`
	SessionID               string         `db:"session_id"`
	UserID                  string         `db:"user_id"`
	TraceID                 string         `db:"trace_id"`
	TriggerKind             string         `db:"trigger_kind"`
	TriggerTokens           int            `db:"trigger_tokens"`
	Content                 string         `db:"content"`
	BoundaryMsgTime         time.Time      `db:"boundary_msg_time"`
	BoundaryMsgIndex        int            `db:"boundary_msg_index"`
	BoundaryMsgID           int64          `db:"boundary_msg_id"`
	ShadowedTokens          int            `db:"shadowed_tokens"`
	SummaryModel            string         `db:"summary_model"`
	SummaryPromptTokens     int            `db:"summary_prompt_tokens"`
	SummaryCompletionTokens int            `db:"summary_completion_tokens"`
	RetainedResultsJSON     []byte         `db:"retained_results_json"`
	MeasurementBasis        sql.NullString `db:"measurement_basis"`
	PreCompactionTokens     sql.NullInt64  `db:"pre_compaction_tokens"`
	InputBudgetTokens       sql.NullInt64  `db:"input_budget_tokens"`
	PostCompactionTokens    sql.NullInt64  `db:"post_compaction_tokens"`
	RetainedTailMessages    sql.NullInt64  `db:"retained_tail_messages"`
	SummaryStrategy         sql.NullString `db:"summary_strategy"`
	ToolSummaryCount        sql.NullInt64  `db:"tool_summary_count"`
	ToolActivityCount       sql.NullInt64  `db:"tool_activity_count"`
	CreateTime              time.Time      `db:"create_time"`
}

func (r compactionRecordRow) model() model.ContextCompaction {
	return model.ContextCompaction{
		ID:                      r.ID,
		SessionID:               r.SessionID,
		UserID:                  r.UserID,
		TraceID:                 r.TraceID,
		TriggerKind:             r.TriggerKind,
		TriggerTokens:           r.TriggerTokens,
		Content:                 r.Content,
		BoundaryMsgTime:         r.BoundaryMsgTime,
		BoundaryMsgIndex:        r.BoundaryMsgIndex,
		BoundaryMsgID:           r.BoundaryMsgID,
		ShadowedTokens:          r.ShadowedTokens,
		SummaryModel:            r.SummaryModel,
		SummaryPromptTokens:     r.SummaryPromptTokens,
		SummaryCompletionTokens: r.SummaryCompletionTokens,
		RetainedResultsJSON:     r.RetainedResultsJSON,
		MeasurementBasis:        r.MeasurementBasis.String,
		PreCompactionTokens:     int(r.PreCompactionTokens.Int64),
		InputBudgetTokens:       int(r.InputBudgetTokens.Int64),
		PostCompactionTokens:    int(r.PostCompactionTokens.Int64),
		RetainedTailMessages:    int(r.RetainedTailMessages.Int64),
		SummaryStrategy:         r.SummaryStrategy.String,
		ToolSummaryCount:        int(r.ToolSummaryCount.Int64),
		ToolActivityCount:       int(r.ToolActivityCount.Int64),
		CreateTime:              r.CreateTime,
	}
}

// LatestCompaction returns the newest compaction record for sessionID, or
// (nil, nil) when the session has never been compacted.
func (s *Store) LatestCompaction(ctx context.Context, sessionID string) (*model.ContextCompaction, error) {
	logQuery(ctx, "compaction.latest", latestCompactionSQL, sessionID)

	var row compactionRecordRow
	err := s.db.GetContext(ctx, &row, latestCompactionSQL, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec := row.model()
	rec.DecodeRetainedResults()
	return &rec, nil
}

// updateSessionCompactedSQL sets the stitching gate. Idempotent by
// construction; the flag is never reset to 0 anywhere.
const updateSessionCompactedSQL = `
UPDATE sessions SET context_compacted = 1 WHERE session_id = ?
`

// UpdateSessionCompacted marks the session as compacted. It is the LAST step
// of the compaction write order (record insert → Redis cache → this flag) so
// a crash mid-sequence leaves at worst a harmless orphan record with the flag
// still 0, never a flagged session with no recoverable record.
func (s *Store) UpdateSessionCompacted(ctx context.Context, sessionID string) error {
	logQuery(ctx, "compaction.update_session_flag", updateSessionCompactedSQL, sessionID)
	_, err := s.db.ExecContext(ctx, updateSessionCompactedSQL, sessionID)
	return err
}

// latestContextTokensSQL returns the most recent turn's end-of-turn context
// size for the turn-start preventive compaction check. Ordered by id DESC (the
// insert order of turn_usage rows), not by created_at, so same-millisecond
// turns still resolve deterministically.
const latestContextTokensSQL = `
SELECT context_tokens
FROM turn_usage
WHERE session_id = ?
ORDER BY id DESC
LIMIT 1
`

// LatestContextTokens returns the newest turn_usage.context_tokens value for
// sessionID, or (0, nil) when the session has no recorded turns (new session,
// or rows written before migration 013) — zero never triggers compaction, so
// the first turn of a session is naturally exempt.
func (s *Store) LatestContextTokens(ctx context.Context, sessionID string) (int, error) {
	logQuery(ctx, "compaction.latest_context_tokens", latestContextTokensSQL, sessionID)

	var tokens int
	err := s.db.GetContext(ctx, &tokens, latestContextTokensSQL, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return tokens, nil
}
