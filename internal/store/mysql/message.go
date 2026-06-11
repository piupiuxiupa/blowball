package mysql

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
)

// appendMessageSQL inserts a new message row. id is AUTO_INCREMENT and is
// returned to the caller via AppendMessage. msg_index is supplied by the
// service layer which tracks the per-session counter.
const appendMessageSQL = `
INSERT INTO messages (session_id, msg_time, agent, msg_index, role, content, trace_id)
VALUES (:session_id, :msg_time, :agent, :msg_index, :role, :content, :trace_id)
`

// listMessagesSQL returns every message for sessionID in msg_index order. The
// covering index idx_messages_session_index makes this a single index walk.
const listMessagesSQL = `
SELECT id, session_id, msg_time, agent, msg_index, role, content, trace_id, update_time
FROM messages
WHERE session_id = ?
ORDER BY msg_index ASC
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

// ListMessages returns every message for sessionID in msg_index order, or an
// empty (non-nil) slice when the session has no messages.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	logQuery(ctx, "message.list", listMessagesSQL, sessionID)

	var messages []model.Message
	if err := s.db.SelectContext(ctx, &messages, listMessagesSQL, sessionID); err != nil {
		return nil, err
	}
	return messages, nil
}
