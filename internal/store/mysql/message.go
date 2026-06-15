package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/cursor"
)

// appendMessageSQL inserts a new message row. id is AUTO_INCREMENT and is
// returned to the caller via AppendMessage. msg_index is supplied by the
// service layer which tracks the per-turn counter.
const appendMessageSQL = `
INSERT INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id)
VALUES (:session_id, :msg_time, :agent, :msg_index, :role, :event_type, :content, :trace_id)
`

// appendMessagesSQL inserts multiple message rows in a single statement. The
// VALUES clause is expanded at runtime by AppendMessages.
const appendMessagesSQL = `
INSERT INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id)
VALUES %s
`

// listMessagesSQL returns every message for sessionID in (msg_time, msg_index)
// order. The covering index idx_messages_session_time makes the leading
// msg_time sort efficient; msg_index resolves ties within a single batch.
const listMessagesSQL = `
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time
FROM messages
WHERE session_id = ?
ORDER BY msg_time ASC, msg_index ASC
`

// AppendMessage inserts m into the messages table and returns the
// auto-incremented id assigned by MySQL.
func (s *Store) AppendMessage(ctx context.Context, m model.Message) (int64, error) {
	logQuery(ctx, "message.append", appendMessageSQL)

	res, err := sqlx.NamedExecContext(ctx, s.db, appendMessageSQL, m)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AppendMessages inserts msgs in a single multi-value INSERT and returns the
// auto-incremented ids assigned by MySQL, in the same order as msgs.
func (s *Store) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	if len(msgs) == 0 {
		return []int64{}, nil
	}

	placeholders := make([]string, 0, len(msgs))
	args := make([]any, 0, len(msgs)*8)
	for _, m := range msgs {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			m.SessionID,
			m.MsgTime,
			m.Agent,
			m.MsgIndex,
			m.Role,
			m.EventType,
			m.Content,
			m.TraceID,
		)
	}

	query := fmt.Sprintf(appendMessagesSQL, strings.Join(placeholders, ", "))
	logQuery(ctx, "message.append_batch", query)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	firstID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	ids := make([]int64, 0, affected)
	for i := range affected {
		ids = append(ids, firstID+i)
	}
	return ids, nil
}

// listMessagesPagedAscSQL returns messages after the cursor ordered by
// (msg_time, msg_index, id) ascending. The id tie-breaker makes the cursor
// stable when two rows share the same msg_time and msg_index.
const listMessagesPagedAscSQL = `
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time
FROM messages
WHERE session_id = ?
  AND (msg_time, msg_index, id) > (?, ?, ?)
ORDER BY msg_time ASC, msg_index ASC, id ASC
LIMIT ?
`

// listMessagesPagedDescSQL returns messages before the cursor ordered by
// (msg_time, msg_index, id) descending.
const listMessagesPagedDescSQL = `
SELECT id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, update_time
FROM messages
WHERE session_id = ?
  AND (msg_time, msg_index, id) < (?, ?, ?)
ORDER BY msg_time DESC, msg_index DESC, id DESC
LIMIT ?
`

// ListMessages returns every message for sessionID in (msg_time, msg_index)
// order, or an empty (non-nil) slice when the session has no messages.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	logQuery(ctx, "message.list", listMessagesSQL, sessionID)

	var messages []model.Message
	if err := s.db.SelectContext(ctx, &messages, listMessagesSQL, sessionID); err != nil {
		return nil, err
	}
	return messages, nil
}

// ListMessagesPaged returns a page of messages for sessionID ordered by
// (msg_time, msg_index, id). order must be "asc" or "desc"; any other value
// defaults to "asc". pageSize is clamped to [1, 200]. An empty cursor requests
// the first page. The returned nextCursor is empty when no further pages exist.
func (s *Store) ListMessagesPaged(ctx context.Context, sessionID, cursorStr string, pageSize int, order string) ([]model.Message, string, error) {
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}

	cur, err := cursor.Decode(cursorStr)
	if err != nil {
		return nil, "", fmt.Errorf("message.list_paged: decode cursor: %w", err)
	}

	var query string
	switch order {
	case "desc":
		query = listMessagesPagedDescSQL
	default:
		query = listMessagesPagedAscSQL
	}

	logQuery(ctx, "message.list_paged", query, sessionID, cur.MsgTime, cur.MsgIndex, cur.ID, pageSize)

	var messages []model.Message
	if err := s.db.SelectContext(ctx, &messages, query, sessionID, cur.MsgTime, cur.MsgIndex, cur.ID, pageSize); err != nil {
		return nil, "", err
	}

	if len(messages) == 0 {
		return messages, "", nil
	}

	last := messages[len(messages)-1]
	nextCur := cursor.Cursor{
		MsgTime:  last.MsgTime,
		MsgIndex: last.MsgIndex,
		ID:       last.ID,
	}
	nextToken, err := cursor.Encode(nextCur)
	if err != nil {
		return nil, "", fmt.Errorf("message.list_paged: encode next cursor: %w", err)
	}
	return messages, nextToken, nil
}
