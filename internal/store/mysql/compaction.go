package mysql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lush/blowball/internal/model"
)

// insertCompactionSQL appends one compaction record. The table is append-only
// with latest-wins stitching: the record's id participates in the
// (session_id, id) ordering that LatestCompaction reads back.
const insertCompactionSQL = `
INSERT INTO context_compactions (
    session_id, user_id, trace_id, trigger_kind, trigger_tokens, content,
    boundary_msg_time, boundary_msg_index, boundary_msg_id,
    shadowed_tokens, summary_model, summary_prompt_tokens, summary_completion_tokens
) VALUES (
    :session_id, :user_id, :trace_id, :trigger_kind, :trigger_tokens, :content,
    :boundary_msg_time, :boundary_msg_index, :boundary_msg_id,
    :shadowed_tokens, :summary_model, :summary_prompt_tokens, :summary_completion_tokens
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
       create_time
FROM context_compactions
WHERE session_id = ?
ORDER BY id DESC
LIMIT 1
`

// LatestCompaction returns the newest compaction record for sessionID, or
// (nil, nil) when the session has never been compacted.
func (s *Store) LatestCompaction(ctx context.Context, sessionID string) (*model.ContextCompaction, error) {
	logQuery(ctx, "compaction.latest", latestCompactionSQL, sessionID)

	var rec model.ContextCompaction
	err := s.db.GetContext(ctx, &rec, latestCompactionSQL, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
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
