package mysql

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/cursor"
)

// appendMessageColumns is the column list shared by the single-row and batch
// inserts. The table name is injected from the fixed Tables value.
const appendMessageColumns = `(session_id, msg_time, agent, msg_index, tool_call_id, role, event_type, status, content, attachments_json, reasoning_content, trace_id, client_msg_id, run_id, agent_instance_id)`

const appendMessageValues = `(:session_id, :msg_time, :agent, :msg_index, :round_no, :role, :event_type, :status, :content, :attachments_json, :reasoning_content, :trace_id, :client_msg_id, :run_id, :agent_instance_id)`

func appendMessageSQL(t Tables) string {
	return `INSERT IGNORE INTO ` + t.Messages + ` ` + appendMessageColumns + ` VALUES ` + appendMessageValues
}

func appendMessagesSQL(t Tables) string {
	return `INSERT IGNORE INTO ` + t.Messages + ` ` + appendMessageColumns + ` VALUES %s`
}

// listMessagesSQL returns every message for sessionID in (msg_time, msg_index)
// order. NULL-tolerance columns are COALESCEd so the plain model string fields
// scan cleanly.
func listMessagesSQL(t Tables) string {
	return `SELECT msg.id as id, session_id, msg_time, agent, msg_index, COALESCE(tool_call_id, '') AS tool_call_id, role, event_type, COALESCE(status, '') AS status, content, attachments_json, COALESCE(reasoning_content, '') AS reasoning_content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM ` + t.Messages + ` msg
INNER JOIN
(
	SELECT id FROM ` + t.Messages + `
	WHERE session_id = ?
	ORDER BY msg_time ASC, msg_index ASC
) AS sub ON msg.id = sub.id
ORDER BY msg.msg_time ASC, msg.msg_index ASC, msg.id ASC`
}

// nilIfEmpty maps an absent client_msg_id (legacy rows / callers that predate
// the idempotency key) onto SQL NULL. run_id and agent_instance_id share the
// mapping for the plain NULL-tolerance reason.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AppendMessage inserts m into this view's message table and returns the
// auto-incremented id. INSERT IGNORE collapses a redelivered client_msg_id.
func (s *SessionStore) AppendMessage(ctx context.Context, m model.Message) (int64, error) {
	query := appendMessageSQL(s.tables)
	logQuery(ctx, "message.append", query)

	params := map[string]any{
		"session_id":        m.SessionID,
		"msg_time":          m.MsgTime,
		"agent":             m.Agent,
		"msg_index":         m.MsgIndex,
		"tool_call_id":      nilIfEmpty(m.ToolCallID),
		"role":              m.Role,
		"event_type":        m.EventType,
		"status":            nilIfEmpty(m.Status),
		"content":           m.Content,
		"attachments_json":  model.MarshalAttachmentRefs(m.Attachments),
		"reasoning_content": nilIfEmpty(m.ReasoningContent),
		"trace_id":          m.TraceID,
		"client_msg_id":     nilIfEmpty(m.ClientMsgID),
		"run_id":            nilIfEmpty(m.RunID),
		"agent_instance_id": nilIfEmpty(m.AgentInstanceID),
	}

	res, err := sqlx.NamedExecContext(ctx, s.db, query, params)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AppendMessages inserts msgs in a single multi-value INSERT.
func (s *SessionStore) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	return appendMessages(ctx, s.db, s.tables, msgs)
}

// appendMessages is the ExtContext core shared with the round-batch
// transaction.
func appendMessages(ctx context.Context, q sqlx.ExtContext, tables Tables, msgs []model.Message) ([]int64, error) {
	if len(msgs) == 0 {
		return []int64{}, nil
	}

	placeholders := make([]string, 0, len(msgs))
	args := make([]any, 0, len(msgs)*15)
	for _, m := range msgs {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			m.SessionID,
			m.MsgTime,
			m.Agent,
			m.MsgIndex,
			nilIfEmpty(m.ToolCallID),
			m.Role,
			m.EventType,
			nilIfEmpty(m.Status),
			m.Content,
			model.MarshalAttachmentRefs(m.Attachments),
			nilIfEmpty(m.ReasoningContent),
			m.TraceID,
			nilIfEmpty(m.ClientMsgID),
			nilIfEmpty(m.RunID),
			nilIfEmpty(m.AgentInstanceID),
		)
	}

	query := fmt.Sprintf(appendMessagesSQL(tables), strings.Join(placeholders, ", "))
	logQuery(ctx, "message.append_batch", query)

	res, err := q.ExecContext(ctx, query, args...)
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

func listMessagesPagedAscSQL(t Tables) string {
	return `SELECT msg.id as id, session_id, msg_time, agent, msg_index, COALESCE(tool_call_id, '') AS tool_call_id, role, event_type, COALESCE(status, '') AS status, content, attachments_json, COALESCE(reasoning_content, '') AS reasoning_content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM ` + t.Messages + ` msg
INNER JOIN
(
	SELECT id FROM ` + t.Messages + `
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
	ORDER BY msg_time ASC, msg_index ASC, id ASC
	LIMIT ?
) AS sub ON msg.id = sub.id`
}

func listMessagesPagedDescSQL(t Tables) string {
	return `SELECT msg.id as id, session_id, msg_time, agent, msg_index, COALESCE(tool_call_id, '') AS tool_call_id, role, event_type, COALESCE(status, '') AS status, content, attachments_json, COALESCE(reasoning_content, '') AS reasoning_content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM
(
	SELECT id FROM ` + t.Messages + `
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
	ORDER BY msg_time DESC, msg_index DESC, id DESC
	LIMIT ?
) AS sub STRAIGHT_JOIN ` + t.Messages + ` msg ON msg.id = sub.id`
}

// subagentPlaceholderFilter restricts a message listing to the placeholder
// sub-agent view, applied inside the inner subselect so filtering happens
// BEFORE pagination. The correlated EXISTS reads the same source's sub-agent
// run table, so a scheduled session never consults ordinary rows.
func subagentPlaceholderFilter(t Tables) string {
	return `
		AND (
			agent_instance_id IS NULL
			OR tool_call_id IS NOT NULL
			OR event_type IN ('agent_start', 'agent_end', 'agent_error')
		)
		AND NOT (
			event_type = 'tool_result'
			AND agent_instance_id IS NULL
			AND JSON_VALID(messages.content)
			AND EXISTS (
				SELECT 1 FROM ` + t.SubAgentRuns + ` sr
				WHERE sr.session_id = messages.session_id
					AND sr.run_id = JSON_UNQUOTE(JSON_EXTRACT(messages.content, '$.tool_call_id'))
			)
		)`
}

func listMessagesPagedPlaceholderAscSQL(t Tables) string {
	return `SELECT msg.id as id, session_id, msg_time, agent, msg_index, COALESCE(tool_call_id, '') AS tool_call_id, role, event_type, COALESCE(status, '') AS status, content, attachments_json, COALESCE(reasoning_content, '') AS reasoning_content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM ` + t.Messages + ` msg
INNER JOIN
(
	SELECT id FROM ` + t.Messages + `
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
` + subagentPlaceholderFilter(t) + `
	ORDER BY msg_time ASC, msg_index ASC, id ASC
	LIMIT ?
) AS sub ON msg.id = sub.id`
}

func listMessagesPagedPlaceholderDescSQL(t Tables) string {
	return `SELECT msg.id as id, session_id, msg_time, agent, msg_index, COALESCE(tool_call_id, '') AS tool_call_id, role, event_type, COALESCE(status, '') AS status, content, attachments_json, COALESCE(reasoning_content, '') AS reasoning_content, trace_id, COALESCE(client_msg_id, '') AS client_msg_id, COALESCE(run_id, '') AS run_id, COALESCE(agent_instance_id, '') AS agent_instance_id, update_time
FROM
(
	SELECT id FROM ` + t.Messages + `
	WHERE session_id = ?
		AND (msg_time, msg_index, id) > (?, ?, ?)
` + subagentPlaceholderFilter(t) + `
	ORDER BY msg_time DESC, msg_index DESC, id DESC
	LIMIT ?
) AS sub STRAIGHT_JOIN ` + t.Messages + ` msg ON msg.id = sub.id`
}

// ListMessages returns every message for sessionID in (msg_time, msg_index)
// order, or an empty (non-nil) slice when the session has no messages.
func (s *SessionStore) ListMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	query := listMessagesSQL(s.tables)
	logQuery(ctx, "message.list", query, sessionID)

	var messages []model.Message
	if err := s.db.SelectContext(ctx, &messages, query, sessionID); err != nil {
		return nil, err
	}
	hydrateAttachments(messages)
	return messages, nil
}

// hydrateAttachments fills each row's Attachments slice from its
// attachments_json payload. A NULL/empty/malformed payload leaves it nil.
func hydrateAttachments(msgs []model.Message) {
	for i := range msgs {
		msgs[i].Attachments = model.UnmarshalAttachmentRefs(msgs[i].AttachmentsJSON)
	}
}

// ListMessagesPaged returns a page of messages for sessionID ordered by
// (msg_time, msg_index, id). order must be "asc" or "desc"; any other value
// defaults to "asc". pageSize is clamped to [1, 5000].
func (s *SessionStore) ListMessagesPaged(ctx context.Context, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error) {
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
		query = listMessagesPagedPlaceholderDescSQL(s.tables)
	case subagentContent == model.SubagentContentPlaceholder:
		query = listMessagesPagedPlaceholderAscSQL(s.tables)
	case order == "desc":
		query = listMessagesPagedDescSQL(s.tables)
	default:
		query = listMessagesPagedAscSQL(s.tables)
	}

	logQuery(ctx, "message.list_paged", query, sessionID, cur.MsgTime, cur.MsgIndex, cur.ID, pageSize)

	var messages []model.Message
	if err := s.db.SelectContext(ctx, &messages, query, sessionID, cur.MsgTime, cur.MsgIndex, cur.ID, pageSize); err != nil {
		return nil, "", err
	}
	hydrateAttachments(messages)

	if len(messages) == 0 {
		return messages, "", nil
	}

	last := messages[len(messages)-1]
	nextToken, err := cursor.Encode(cursor.Cursor{MsgTime: last.MsgTime, MsgIndex: last.MsgIndex, ID: last.ID})
	if err != nil {
		return nil, "", fmt.Errorf("message.list_paged: encode next cursor: %w", err)
	}
	return messages, nextToken, nil
}

// ── interactive-source compatibility wrappers ──────────────────────────────

// AppendMessage inserts into the interactive messages table.
func (s *Store) AppendMessage(ctx context.Context, m model.Message) (int64, error) {
	return s.ForSource(model.SessionSourceInteractive).AppendMessage(ctx, m)
}

// AppendMessages inserts into the interactive messages table.
func (s *Store) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	return s.ForSource(model.SessionSourceInteractive).AppendMessages(ctx, msgs)
}

// ListMessages reads the interactive messages table.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]model.Message, error) {
	return s.ForSource(model.SessionSourceInteractive).ListMessages(ctx, sessionID)
}

// ListMessagesPaged reads a page from the interactive messages table.
func (s *Store) ListMessagesPaged(ctx context.Context, sessionID, cursorStr string, pageSize int, order, subagentContent string) ([]model.Message, string, error) {
	return s.ForSource(model.SessionSourceInteractive).ListMessagesPaged(ctx, sessionID, cursorStr, pageSize, order, subagentContent)
}
