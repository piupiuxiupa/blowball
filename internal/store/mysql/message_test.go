package mysql

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// testDSN returns the MySQL DSN from the environment, or an empty string when
// no MySQL instance is available for testing.
func testDSN() string {
	return os.Getenv("MYSQL_TEST_DSN")
}

// setupTestStore opens a connection and creates the messages table. Tests are
// skipped when MYSQL_TEST_DSN is not set.
func setupTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}

	store, err := New(dsn)
	require.NoError(t, err)

	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS messages")
		_ = store.Close()
	}

	// Create an isolated messages table without the FK constraint so tests do
	// not require a sessions row. The column layout mirrors the production
	// schema (migrations 004 + 005 + 012 + 014: nullable role, event_type,
	// millisecond msg_time, nullable client_msg_id with its UNIQUE index,
	// nullable run_id/agent_instance_id) so the SELECT scan paths run against the real shape —
	// this is exactly how the legacy-NULL scan bug escaped: an out-of-date
	// test table.
	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS messages (
			id          BIGINT       NOT NULL AUTO_INCREMENT,
			session_id  CHAR(36)     NOT NULL,
			msg_time    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			agent       VARCHAR(32)  NOT NULL,
			msg_index   INT          NOT NULL,
			role        VARCHAR(16)  NULL,
			event_type  VARCHAR(16)  NOT NULL DEFAULT 'token',
			content     MEDIUMTEXT   NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			client_msg_id CHAR(36)   NULL,
			run_id      CHAR(64)     NULL,
			agent_instance_id CHAR(24) NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			KEY idx_messages_session_time (session_id, msg_time),
			UNIQUE KEY uk_messages_client_msg_id (client_msg_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)

	return store, cleanup
}

// insertTestMessage inserts a row into messages and returns its id.
func insertTestMessage(t *testing.T, s *Store, sessionID string, msgTime time.Time, msgIndex int, content string) int64 {
	t.Helper()
	res, err := s.db.ExecContext(context.Background(), `
		INSERT INTO messages (session_id, msg_time, agent, msg_index, role, content, trace_id)
		VALUES (?, ?, 'user', ?, 'user', ?, 'trace-1')
	`, sessionID, msgTime, msgIndex, content)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestListMessagesPaged_AscendingFirstPage verifies the default ascending
// order, that next_page_token is present when more rows exist, and that the
// cursor advances to the next page. The real store emits a token whenever it
// returns rows (the caller detects the end by the NEXT page coming back
// empty) — this env-gated test previously asserted a "no token on the last
// non-empty page" contract the store has never implemented, so it had never
// actually passed against MySQL.
func TestListMessagesPaged_AscendingFirstPage(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-000000000001"
	base := time.Unix(1_700_000_000, 0).UTC()
	id1 := insertTestMessage(t, store, sessionID, base, 0, "first")
	insertTestMessage(t, store, sessionID, base.Add(time.Second), 0, "second")

	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 1, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, id1, msgs[0].ID)
	assert.Equal(t, "first", msgs[0].Content)
	assert.NotEmpty(t, next, "next_page_token must be present when more rows exist")

	msgs2, next2, err := store.ListMessagesPaged(context.Background(), sessionID, next, 10, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, msgs2, 1)
	assert.Equal(t, "second", msgs2[0].Content)
	require.NotEmpty(t, next2, "the store always emits a token with non-empty pages")

	// The end of the session is signalled by the page AFTER the last row:
	// empty page, no token.
	msgs3, next3, err := store.ListMessagesPaged(context.Background(), sessionID, next2, 10, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	assert.Empty(t, msgs3, "page past the last row must be empty")
	assert.Empty(t, next3, "empty page must carry no next_page_token")
}

// TestListMessagesPaged_DescendingOrder verifies descending pagination.
func TestListMessagesPaged_DescendingOrder(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-000000000002"
	base := time.Unix(1_700_000_000, 0).UTC()
	insertTestMessage(t, store, sessionID, base, 0, "first")
	insertTestMessage(t, store, sessionID, base.Add(time.Second), 0, "second")
	insertTestMessage(t, store, sessionID, base.Add(2*time.Second), 0, "third")

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "desc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "third", msgs[0].Content)
	assert.Equal(t, "second", msgs[1].Content)
	assert.Equal(t, "first", msgs[2].Content)
}

// TestListMessagesPaged_TieBreakerByID verifies that rows with identical
// (msg_time, msg_index) are ordered and paged by id.
func TestListMessagesPaged_TieBreakerByID(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-000000000003"
	base := time.Unix(1_700_000_000, 0).UTC()
	id1 := insertTestMessage(t, store, sessionID, base, 0, "a")
	id2 := insertTestMessage(t, store, sessionID, base, 0, "b")

	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 1, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, id1, msgs[0].ID)
	assert.NotEmpty(t, next)

	msgs2, _, err := store.ListMessagesPaged(context.Background(), sessionID, next, 1, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, msgs2, 1)
	assert.Equal(t, id2, msgs2[0].ID)
}

// TestListMessagesPaged_EmptySession_ReturnsEmptySlice verifies an empty
// session returns an empty (non-nil) result set with no next cursor.
func TestListMessagesPaged_EmptySession_ReturnsEmptySlice(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-000000000004"
	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	assert.Empty(t, msgs)
	assert.Empty(t, next)
}

// TestListMessagesPaged_PageSizeClamped verifies page_size is bounded.
func TestListMessagesPaged_PageSizeClamped(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-000000000005"
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 5; i++ {
		insertTestMessage(t, store, sessionID, base.Add(time.Duration(i)*time.Second), 0, "msg")
	}

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 0, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	assert.Len(t, msgs, 1, "page_size <= 0 must clamp to 1")

	msgs, _, err = store.ListMessagesPaged(context.Background(), sessionID, "", 1000, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	assert.Len(t, msgs, 5, "page_size > max must clamp to max (200), returning all rows here")
}

// TestListMessagesPaged_InvalidCursor_ReturnsError verifies that a malformed
// page_token is rejected.
func TestListMessagesPaged_InvalidCursor_ReturnsError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	_, _, err := store.ListMessagesPaged(context.Background(), "aaaaaaaa-0000-7000-8000-000000000006", "not-a-token", 10, "asc", model.SubagentContentFull)
	require.Error(t, err)
}

// TestListMessages_NullClientMsgIDScansAsEmptyString is the regression test
// for the legacy-row read path: rows written before migration 012 carry NULL
// in client_msg_id, and the SELECTs COALESCE it to the empty string so sqlx
// can scan it into the model's plain string field. Without the COALESCE every
// read of a legacy session failed with "converting NULL to string is
// unsupported".
func TestListMessages_NullClientMsgIDScansAsEmptyString(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-0000000000aa"
	base := time.Unix(1_700_000_000, 0).UTC()
	// One legacy row (client_msg_id NULL, the pre-012 shape) and one stamped
	// row written through the store API.
	legacyID := insertTestMessage(t, store, sessionID, base, 0, "legacy")
	stamped := model.Message{
		SessionID:   sessionID,
		MsgTime:     base.Add(time.Second),
		Agent:       model.AgentUser,
		MsgIndex:    1,
		Role:        model.RoleUser,
		EventType:   model.EventTypeMessage,
		Content:     "stamped",
		TraceID:     "trace-1",
		ClientMsgID: "019a0000-0000-7000-8000-000000000001",
	}
	_, err := store.AppendMessages(context.Background(), []model.Message{stamped})
	require.NoError(t, err)

	msgs, err := store.ListMessages(context.Background(), sessionID)
	require.NoError(t, err, "legacy NULL client_msg_id rows must scan cleanly")
	require.Len(t, msgs, 2)
	assert.Equal(t, legacyID, msgs[0].ID)
	assert.Empty(t, msgs[0].ClientMsgID, "NULL must surface as the empty string")
	assert.Equal(t, stamped.ClientMsgID, msgs[1].ClientMsgID)

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc", model.SubagentContentFull)
	require.NoError(t, err, "paged read must scan legacy NULL rows cleanly too")
	require.Len(t, paged, 2)
	assert.Empty(t, paged[0].ClientMsgID)
	assert.Equal(t, stamped.ClientMsgID, paged[1].ClientMsgID)
}

// TestAppendMessages_EmptyClientMsgIDWritesNull verifies the inverse mapping:
// an unstamped message writes SQL NULL (not the empty string), so two such
// rows never collide on uk_messages_client_msg_id.
func TestAppendMessages_EmptyClientMsgIDWritesNull(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-0000000000bb"
	base := time.Unix(1_700_000_000, 0).UTC()
	two := []model.Message{
		{SessionID: sessionID, MsgTime: base, Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "a", TraceID: "t"},
		{SessionID: sessionID, MsgTime: base.Add(time.Second), Agent: model.AgentUser, MsgIndex: 1, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "b", TraceID: "t"},
	}
	_, err := store.AppendMessages(context.Background(), two)
	require.NoError(t, err, "two unstamped rows must coexist (NULL repeats freely)")

	var nulls int
	require.NoError(t, store.db.GetContext(context.Background(), &nulls,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND client_msg_id IS NULL`, sessionID))
	assert.Equal(t, 2, nulls, "unstamped messages must persist as NULL, not ''")
}

// TestMessages_RunIDRoundTrip verifies the sub-agent run identity column
// (migration 014): a row stamped with RunID survives the write→read round
// trip on every read path, and rows written without one (pre-change shape,
// direct SQL insert) read back as the empty string — NULL tolerance.
func TestMessages_RunIDRoundTrip(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-0000000000cc"
	base := time.Unix(1_700_000_000, 0).UTC()
	// A legacy-shaped row inserted without run_id.
	legacyID := insertTestMessage(t, store, sessionID, base, 0, "legacy")
	// A stamped sub-agent row written through the store API.
	stamped := model.Message{
		SessionID:   sessionID,
		MsgTime:     base.Add(time.Second),
		Agent:       model.AgentChongzhi,
		MsgIndex:    1,
		Role:        model.RoleAssistant,
		EventType:   model.EventTypeToken,
		Content:     "interleaved fragment",
		TraceID:     "trace-1",
		ClientMsgID: "019a0000-0000-7000-8000-000000000002",
		RunID:       "call_x1",
	}
	_, err := store.AppendMessages(context.Background(), []model.Message{stamped})
	require.NoError(t, err)

	msgs, err := store.ListMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, legacyID, msgs[0].ID)
	assert.Empty(t, msgs[0].RunID, "legacy NULL run_id must surface as the empty string")
	assert.Equal(t, "call_x1", msgs[1].RunID, "stamped run_id must round-trip")

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, paged, 2)
	assert.Empty(t, paged[0].RunID)
	assert.Equal(t, "call_x1", paged[1].RunID)

	// An unstamped model row writes SQL NULL (empty string would be a
	// meaningless sentinel indistinguishable from a real id of "").
	var nullRuns int
	require.NoError(t, store.db.GetContext(context.Background(), &nullRuns,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND run_id IS NULL`, sessionID))
	assert.Equal(t, 1, nullRuns, "unstamped run_id must persist as NULL")
}

func TestMessages_AgentInstanceIDRoundTrip(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-0000000000ad"
	msg := model.Message{
		SessionID:       sessionID,
		MsgTime:         time.Now().UTC(),
		Agent:           "Subagent",
		MsgIndex:        1,
		Role:            model.RoleAssistant,
		EventType:       model.EventTypeToken,
		Content:         "hi",
		TraceID:         "trace-1",
		ClientMsgID:     "019a0000-0000-7000-8000-0000000000ad",
		RunID:           "call_x1",
		AgentInstanceID: "w-abc123",
	}
	_, err := store.AppendMessages(context.Background(), []model.Message{msg})
	require.NoError(t, err)

	msgs, err := store.ListMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "w-abc123", msgs[0].AgentInstanceID)

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	require.Len(t, paged, 1)
	assert.Equal(t, "w-abc123", paged[0].AgentInstanceID)

	var nullInstances int
	require.NoError(t, store.db.GetContext(context.Background(), &nullInstances,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND agent_instance_id IS NULL`, sessionID))
	assert.Equal(t, 0, nullInstances)
}

// setupTestStoreWithSubAgentTables creates the messages table AND the
// subagent_instances/subagent_runs tables (via setupSubAgentRunStore's shape):
// the placeholder-view SQL always references subagent_runs in its EXISTS leg,
// so a placeholder read against a store without the table fails. The cleanup
// drops all three tables.
func setupTestStoreWithSubAgentTables(t *testing.T) (*Store, func()) {
	t.Helper()
	if testDSN() == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}
	store, err := New(testDSN())
	require.NoError(t, err)

	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS messages")
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS subagent_runs")
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS subagent_instances")
		_ = store.Close()
	}

	ctx := context.Background()
	_, err = store.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS messages (
			id          BIGINT       NOT NULL AUTO_INCREMENT,
			session_id  CHAR(36)     NOT NULL,
			msg_time    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			agent       VARCHAR(32)  NOT NULL,
			msg_index   INT          NOT NULL,
			role        VARCHAR(16)  NULL,
			event_type  VARCHAR(16)  NOT NULL DEFAULT 'token',
			content     MEDIUMTEXT   NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			client_msg_id CHAR(36)   NULL,
			run_id      CHAR(64)     NULL,
			agent_instance_id CHAR(24) NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			KEY idx_messages_session_time (session_id, msg_time),
			UNIQUE KEY uk_messages_client_msg_id (client_msg_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `
		CREATE TABLE subagent_instances (
			id BIGINT NOT NULL AUTO_INCREMENT,
			session_id CHAR(36) NOT NULL,
			agent_instance_id CHAR(24) NOT NULL,
			parent_instance_id CHAR(24) NULL,
			depth INT UNSIGNED NOT NULL,
			name VARCHAR(128) NOT NULL,
			system_prompt MEDIUMTEXT NOT NULL,
			tools_json JSON NULL,
			latest_run_id CHAR(64) NULL,
			create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			update_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
			PRIMARY KEY (id),
			UNIQUE KEY uk_subagent_instances_session_instance (session_id, agent_instance_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
		CREATE TABLE subagent_runs (
			id BIGINT NOT NULL AUTO_INCREMENT,
			session_id CHAR(36) NOT NULL,
			agent_instance_id CHAR(24) NOT NULL,
			run_id CHAR(64) NOT NULL,
			previous_run_id CHAR(64) NULL,
			run_no INT UNSIGNED NOT NULL,
			parent_instance_id CHAR(24) NULL,
			depth INT UNSIGNED NOT NULL,
			name VARCHAR(128) NOT NULL,
			tools_json JSON NULL,
			status VARCHAR(16) NOT NULL,
			snapshot_kind VARCHAR(16) NOT NULL,
			resume_eligible TINYINT(1) NOT NULL,
			base_message_count INT UNSIGNED NOT NULL,
			message_count INT UNSIGNED NOT NULL,
			context_message_count INT UNSIGNED NOT NULL,
			context_bytes INT UNSIGNED NOT NULL,
			messages_json JSON NOT NULL,
			started_at TIMESTAMP(3) NOT NULL,
			finished_at TIMESTAMP(3) NOT NULL,
			create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			update_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
			PRIMARY KEY (id),
			UNIQUE KEY uk_subagent_runs_session_run (session_id, run_id),
			UNIQUE KEY uk_subagent_runs_instance_run_no (session_id, agent_instance_id, run_no)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	return store, cleanup
}

// placeholderRow is a shortcut for building one messages row of the
// placeholder-filter fixture.
type placeholderRow struct {
	agentInstanceID string
	eventType       string
	content         string
	role            string
	agent           string
}

// insertPlaceholderRow inserts one message row with an explicit event type and
// instance identity (empty instance → SQL NULL, mirroring nilIfEmpty).
func insertPlaceholderRow(t *testing.T, s *Store, sessionID string, msgTime time.Time, msgIndex int, row placeholderRow) int64 {
	t.Helper()
	if row.role == "" {
		row.role = model.RoleAssistant
	}
	if row.agent == "" {
		row.agent = "Subagent"
	}
	res, err := s.db.ExecContext(context.Background(), `
		INSERT INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id, agent_instance_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'trace-1', NULLIF(?, ''))
	`, sessionID, msgTime, row.agent, msgIndex, row.role, row.eventType, row.content, row.agentInstanceID)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// setupPlaceholderFixture builds a messages+subagent_runs store with the
// canonical placeholder-filter conversation:
//
//	idx 0: user message (NULL identity, event message)
//	idx 1: parent spawn_subagent tool_call  (NULL identity, run id "call-spawn-1")
//	idx 2: parent spawn_subagent tool_result (NULL identity, content carries
//	       {"tool_call_id":"call-spawn-1"} — the duplicated child output)
//	idx 3-7: dynamic child rows (instance w-abc123): agent_start, token,
//	       reasoning, tool_call, tool_result
//	idx 8: dynamic child agent_end (instance w-abc123)
//	idx 9: dynamic child agent_error (instance w-abc123 — emitted by a later
//	       dispatch of the same instance)
//	idx 10: parent answer (NULL identity token)
//	idx 11: legacy tool_result (NULL identity, unrelated tool_call_id)
//	idx 12: legacy tool_result (NULL identity) whose tool_call_id collides with
//	       a DIFFERENT session's run id — must stay visible
//
// plus one subagent_runs row keyed run_id = "call-spawn-1" in this session and
// one row keyed "call-1" in another session.
func setupPlaceholderFixture(t *testing.T) (*Store, string, func()) {
	t.Helper()
	store, cleanup := setupTestStoreWithSubAgentTables(t)

	ctx := context.Background()
	sessionID := "aaaaaaaa-0000-7000-8000-00000000f001"
	base := time.Unix(1_700_010_000, 0).UTC()
	rowAt := func(i int, row placeholderRow) {
		insertPlaceholderRow(t, store, sessionID, base.Add(time.Duration(i)*time.Second), i, row)
	}
	rowAt(0, placeholderRow{eventType: model.EventTypeMessage, content: "user question", role: model.RoleUser, agent: model.AgentUser})
	rowAt(1, placeholderRow{eventType: model.EventTypeToolCall, content: `{"tool_call_id":"call-spawn-1","name":"spawn_subagent"}`, agent: model.AgentConfucius})
	rowAt(2, placeholderRow{eventType: model.EventTypeToolResult, content: `{"tool_call_id":"call-spawn-1","output":"child final answer"}`, agent: model.AgentConfucius})
	rowAt(3, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeAgentStart, content: ""})
	rowAt(4, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeToken, content: "child fragment"})
	rowAt(5, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeReasoning, content: "child thinking"})
	rowAt(6, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeToolCall, content: `{"tool_call_id":"child-tc","name":"xizhi_read_file"}`})
	rowAt(7, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeToolResult, content: `{"tool_call_id":"child-tc","output":"file bytes"}`})
	rowAt(8, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeAgentEnd, content: ""})
	rowAt(9, placeholderRow{agentInstanceID: "w-abc123", eventType: model.EventTypeAgentError, content: "boom"})
	rowAt(10, placeholderRow{eventType: model.EventTypeToken, content: "parent answer", agent: model.AgentConfucius})
	rowAt(11, placeholderRow{eventType: model.EventTypeToolResult, content: `{"tool_call_id":"unrelated-tc","output":"kept"}`, agent: model.AgentConfucius})

	// Cross-session decoy: another session reusing the same tool_call id must
	// not cause THIS session's matching tool_result to be omitted (the EXISTS
	// leg is session-scoped).
	rowAt(12, placeholderRow{eventType: model.EventTypeToolResult, content: `{"tool_call_id":"call-1","output":"cross-session kept"}`, agent: model.AgentConfucius})
	otherInstance := testSubAgentInstance("aaaaaaaa-0000-7000-8000-00000000f099", "w-other")
	require.NoError(t, store.AppendSubAgentRun(ctx, otherInstance, testSubAgentRun(otherInstance, "call-1", "", 1)))

	instance := testSubAgentInstance(sessionID, "w-abc123")
	run := testSubAgentRun(instance, "call-spawn-1", "", 1)
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run))

	return store, sessionID, cleanup
}

// placeholderContents maps a page of messages to their contents for compact
// assertions.
func placeholderContents(msgs []model.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

// TestListMessagesPaged_FullModeReturnsEverything verifies the default view is
// untouched by the placeholder filter (unique-subagent-message-placeholders).
func TestListMessagesPaged_FullModeReturnsEverything(t *testing.T) {
	store, sessionID, cleanup := setupPlaceholderFixture(t)
	defer cleanup()

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 50, "asc", model.SubagentContentFull)
	require.NoError(t, err)
	assert.Len(t, msgs, 13, "full mode returns every row, filtered or not")
}

// TestListMessagesPaged_PlaceholderFilter verifies the placeholder view:
// NULL-identity rows and dynamic lifecycle markers survive; dynamic payload
// rows (token/reasoning/tool_call/tool_result) and the parent's duplicated
// spawn tool_result are omitted — while the parent tool_call row stays.
func TestListMessagesPaged_PlaceholderFilter(t *testing.T) {
	store, sessionID, cleanup := setupPlaceholderFixture(t)
	defer cleanup()

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 50, "asc", model.SubagentContentPlaceholder)
	require.NoError(t, err)

	want := []string{
		"user question", // NULL-identity user message
		`{"tool_call_id":"call-spawn-1","name":"spawn_subagent"}`, // parent tool_call stays
		"",              // child agent_start
		"",              // child agent_end
		"boom",          // child agent_error
		"parent answer", // NULL-identity parent token
		`{"tool_call_id":"unrelated-tc","output":"kept"}`,         // unrelated tool_result stays
		`{"tool_call_id":"call-1","output":"cross-session kept"}`, // other session's run must not omit this
	}
	assert.Equal(t, want, placeholderContents(msgs))

	for _, m := range msgs {
		if m.AgentInstanceID != "" {
			assert.Contains(t,
				[]string{model.EventTypeAgentStart, model.EventTypeAgentEnd, model.EventTypeAgentError},
				m.EventType,
				"instance-attributed placeholder rows must be lifecycle markers only")
		}
	}
}

// TestListMessagesPaged_PlaceholderFilterBeforePagination verifies the filter
// is applied BEFORE the LIMIT inside the inner subselect: a page window is
// measured in filtered rows, and the next cursor (built from the last RETURNED
// row) resumes exactly after it without surfacing omitted rows.
func TestListMessagesPaged_PlaceholderFilterBeforePagination(t *testing.T) {
	store, sessionID, cleanup := setupPlaceholderFixture(t)
	defer cleanup()

	ctx := context.Background()
	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		msgs, next, err := store.ListMessagesPaged(ctx, sessionID, cursor, 2, "asc", model.SubagentContentPlaceholder)
		require.NoError(t, err)
		seen = append(seen, placeholderContents(msgs)...)
		if len(msgs) == 0 {
			assert.Empty(t, next)
			break
		}
		require.NotEmpty(t, next, "a non-empty page always carries a next cursor")
		cursor = next
		require.Less(t, page, 10, "pagination must terminate")
	}

	assert.Equal(t, []string{
		"user question",
		`{"tool_call_id":"call-spawn-1","name":"spawn_subagent"}`,
		"", "", "boom",
		"parent answer",
		`{"tool_call_id":"unrelated-tc","output":"kept"}`,
		`{"tool_call_id":"call-1","output":"cross-session kept"}`,
	}, seen, "walked pages must equal the filtered set, in order, with no row surfaced twice")

	// Desc order walks the same filtered set in reverse.
	desc, _, err := store.ListMessagesPaged(ctx, sessionID, "", 50, "desc", model.SubagentContentPlaceholder)
	require.NoError(t, err)
	require.Len(t, desc, 8)
	assert.Equal(t, `{"tool_call_id":"call-1","output":"cross-session kept"}`, desc[0].Content)
	assert.Equal(t, "user question", desc[len(desc)-1].Content)
}

// TestListMessagesPaged_PlaceholderLegacyVisibility verifies rows written
// before the dynamic-subagent capability (no instance identity, no run id,
// plain event types) stay fully visible in placeholder mode — the legacy
// visibility guarantee.
func TestListMessagesPaged_PlaceholderLegacyVisibility(t *testing.T) {
	store, cleanup := setupTestStoreWithSubAgentTables(t)
	defer cleanup()

	sessionID := "aaaaaaaa-0000-7000-8000-00000000f002"
	base := time.Unix(1_700_020_000, 0).UTC()
	for i, content := range []string{"legacy user", "legacy token", "legacy tool_result"} {
		et := model.EventTypeToken
		if i == 2 {
			et = model.EventTypeToolResult
		}
		insertPlaceholderRow(t, store, sessionID, base.Add(time.Duration(i)*time.Second), i,
			placeholderRow{eventType: et, content: content, agent: model.AgentConfucius})
	}

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 50, "asc", model.SubagentContentPlaceholder)
	require.NoError(t, err)
	assert.Equal(t, []string{"legacy user", "legacy token", "legacy tool_result"}, placeholderContents(msgs),
		"NULL-identity legacy rows (including tool_result rows with no matching run) stay visible")
}
