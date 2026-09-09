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
// service layer which tracks the per-turn counter. The INSERT is IGNOREd on a
// duplicate client_msg_id (UNIQUE index uk_messages_client_msg_id) so a
// redelivered write-behind record collapses to the existing row — the
// write-behind queue guarantees at-least-once delivery, the UNIQUE key plus
// IGNORE makes it effectively exactly-once.
const appendMessageSQL = `
INSERT IGNORE INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, client_msg_id, run_id, agent_instance_id)
VALUES (:session_id, :msg_time, :agent, :msg_index, :role, :event_type, :content, :trace_id, :client_msg_id, :run_id, :agent_instance_id)
`

// appendMessagesSQL inserts multiple message rows in a single statement. The
// VALUES clause is expanded at runtime by AppendMessages. INSERT IGNORE for
// the same idempotency reason as appendMessageSQL.
const appendMessagesSQL = `
INSERT IGNORE INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, client_msg_id, run_id, agent_instance_id)
VALUES %s
`

// listMessagesSQL returns every message for sessionID in (msg_time, msg_index)
// order. The covering index idx_messages_session_time makes the leading
// msg_time sort efficient; msg_index resolves ties within a single batch.
// client_msg_id, run_id and agent_instance_id are COALESCEd to the empty
// string: legacy rows written before their migrations carry NULL, and sqlx cannot scan NULL into
// the model's plain string fields (nilIfEmpty performs the inverse
// empty→NULL mapping on write, so the round trip is stable).
const listMessagesSQL = `
SELECT msg.id as id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM messages msg
INNER JOIN 
(
	SELECT id FROM messages
	WHERE session_id = ?
	ORDER BY msg_time ASC, msg_index ASC
) AS sub ON msg.id = sub.id
`

// nilIfEmpty maps an absent client_msg_id (legacy rows / callers that predate
// the idempotency key) onto SQL NULL. Binding the empty string instead would
// violate uk_messages_client_msg_id the moment two such rows coexist, because
// the empty string is a single colliding value while NULL repeats freely.
// run_id and agent_instance_id share the mapping for the plain NULL-tolerance
// reason (migrations 014/016): a non-sub-agent row is NULL, not a colliding
// empty string.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AppendMessage inserts m into the messages table and returns the
// auto-incremented id assigned by MySQL. A row with the same client_msg_id is
// silently ignored (see appendMessageSQL), in which case the returned id is
// that of the pre-existing row's statement outcome and must not be relied on.
func (s *Store) AppendMessage(ctx context.Context, m model.Message) (int64, error) {
	logQuery(ctx, "message.append", appendMessageSQL)

	params := map[string]any{
		"session_id":        m.SessionID,
		"msg_time":          m.MsgTime,
		"agent":             m.Agent,
		"msg_index":         m.MsgIndex,
		"role":              m.Role,
		"event_type":        m.EventType,
		"content":           m.Content,
		"trace_id":          m.TraceID,
		"client_msg_id":     nilIfEmpty(m.ClientMsgID),
		"run_id":            nilIfEmpty(m.RunID),
		"agent_instance_id": nilIfEmpty(m.AgentInstanceID),
	}

	res, err := sqlx.NamedExecContext(ctx, s.db, appendMessageSQL, params)
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
// auto-incremented ids assigned by MySQL, in the same order as msgs. Rows
// whose client_msg_id already exists are ignored (see appendMessagesSQL), so
// under redelivery the id sequence is not meaningful — no caller consumes it.
func (s *Store) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	if len(msgs) == 0 {
		return []int64{}, nil
	}

	placeholders := make([]string, 0, len(msgs))
	args := make([]any, 0, len(msgs)*11)
	for _, m := range msgs {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			m.SessionID,
			m.MsgTime,
			m.Agent,
			m.MsgIndex,
			m.Role,
			m.EventType,
			m.Content,
			m.TraceID,
			nilIfEmpty(m.ClientMsgID),
			nilIfEmpty(m.RunID),
			nilIfEmpty(m.AgentInstanceID),
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
SELECT msg.id as id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM messages msg
INNER JOIN 
(
	SELECT id FROM messages
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
	ORDER BY msg_time ASC, msg_index ASC, id ASC
	LIMIT ?
) AS sub ON msg.id = sub.id
`

// listMessagesPagedDescSQL returns messages before the cursor ordered by
// (msg_time, msg_index, id) descending. The STRAIGHT_JOIN forces the
// materialized subselect to drive the join so its ORDER BY survives as the
// output order — a plain INNER JOIN lets the optimizer flip the join (reading
// the outer table first) and silently lose the descending order on MySQL 9.x.
const listMessagesPagedDescSQL = `
SELECT msg.id as id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM
(
	SELECT id FROM messages
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
	ORDER BY msg_time DESC, msg_index DESC, id DESC
	LIMIT ?
) AS sub STRAIGHT_JOIN messages msg ON msg.id = sub.id
`

// subagentPlaceholderFilter restricts a message listing to the placeholder
// sub-agent view (unique-subagent-message-placeholders capability, applied
// inside the inner subselect so filtering happens BEFORE pagination):
//   - rows with no dynamic instance identity stay visible (main-agent output,
//     user messages, and pre-dynamic-subagent legacy rows);
//   - for rows carrying a dynamic instance identity only the lifecycle markers
//     (agent_start / agent_end / agent_error) stay visible as placeholders —
//     token / reasoning / tool_call / tool_result payload rows are omitted;
//   - the parent's spawn_subagent tool_result row (which duplicates the
//     sub-agent's final output) is omitted by matching its JSON-serialized
//     tool_call_id against subagent_runs.run_id — the parent tool_call row
//     itself stays visible.
//
// The EXISTS leg correlates on messages.session_id so the parent-result
// omission never crosses sessions, and legacy `legacy:<instance>` run ids
// cannot collide with real parent tool_call ids. The JSON_VALID guard keeps
// plain-text legacy tool_result rows (pre-JSON content shapes) visible — and
// the whole read from erroring — instead of failing the query in
// JSON_EXTRACT.
const subagentPlaceholderFilter = `
		AND (
			agent_instance_id IS NULL
			OR event_type IN ('agent_start', 'agent_end', 'agent_error')
		)
		AND NOT (
			event_type = 'tool_result'
			AND agent_instance_id IS NULL
			AND JSON_VALID(messages.content)
			AND EXISTS (
				SELECT 1 FROM subagent_runs sr
				WHERE sr.session_id = messages.session_id
					AND sr.run_id = JSON_UNQUOTE(JSON_EXTRACT(messages.content, '$.tool_call_id'))
			)
		)
`

// listMessagesPagedPlaceholderAscSQL is the placeholder-view twin of
// listMessagesPagedAscSQL.
const listMessagesPagedPlaceholderAscSQL = `
SELECT msg.id as id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM messages msg
INNER JOIN
(
	SELECT id FROM messages
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
` + subagentPlaceholderFilter + `
	ORDER BY msg_time ASC, msg_index ASC, id ASC
	LIMIT ?
) AS sub ON msg.id = sub.id
`

// listMessagesPagedPlaceholderDescSQL is the placeholder-view twin of
// listMessagesPagedDescSQL.
const listMessagesPagedPlaceholderDescSQL = `
SELECT msg.id as id, session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM
(
	SELECT id FROM messages
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
` + subagentPlaceholderFilter + `
	ORDER BY msg_time DESC, msg_index DESC, id DESC
	LIMIT ?
) AS sub STRAIGHT_JOIN messages msg ON msg.id = sub.id
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
// defaults to "asc". pageSize is clamped to [1, 5000]. An empty cursor requests
// the first page. The returned nextCursor is empty when no further pages exist.
// subagentContent selects the sub-agent history view: model.SubagentContentFull
// (or any unknown value — the handler validates the query param before calling)
// is the legacy unfiltered view; model.SubagentContentPlaceholder applies the
// placeholder filter before pagination (see subagentPlaceholderFilter). The
// cursor semantics are unchanged: the next cursor is built from the last
// RETURNED row of the filtered page.
func (s *Store) ListMessagesPaged(ctx context.Context, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error) {
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 5000 {
		pageSize = 5000
	}

	cur, err := cursor.Decode(cursorStr)
	if err != nil {
		return nil, "", fmt.Errorf("message.list_paged: decode cursor: %w", err)
	}

	var query string
	switch {
	case subagentContent == model.SubagentContentPlaceholder && order == "desc":
		query = listMessagesPagedPlaceholderDescSQL
	case subagentContent == model.SubagentContentPlaceholder:
		query = listMessagesPagedPlaceholderAscSQL
	case order == "desc":
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
