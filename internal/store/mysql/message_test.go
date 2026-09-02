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

	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 1, "asc")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, id1, msgs[0].ID)
	assert.Equal(t, "first", msgs[0].Content)
	assert.NotEmpty(t, next, "next_page_token must be present when more rows exist")

	msgs2, next2, err := store.ListMessagesPaged(context.Background(), sessionID, next, 10, "asc")
	require.NoError(t, err)
	require.Len(t, msgs2, 1)
	assert.Equal(t, "second", msgs2[0].Content)
	require.NotEmpty(t, next2, "the store always emits a token with non-empty pages")

	// The end of the session is signalled by the page AFTER the last row:
	// empty page, no token.
	msgs3, next3, err := store.ListMessagesPaged(context.Background(), sessionID, next2, 10, "asc")
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

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "desc")
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

	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 1, "asc")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, id1, msgs[0].ID)
	assert.NotEmpty(t, next)

	msgs2, _, err := store.ListMessagesPaged(context.Background(), sessionID, next, 1, "asc")
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
	msgs, next, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc")
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

	msgs, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 0, "asc")
	require.NoError(t, err)
	assert.Len(t, msgs, 1, "page_size <= 0 must clamp to 1")

	msgs, _, err = store.ListMessagesPaged(context.Background(), sessionID, "", 1000, "asc")
	require.NoError(t, err)
	assert.Len(t, msgs, 5, "page_size > max must clamp to max (200), returning all rows here")
}

// TestListMessagesPaged_InvalidCursor_ReturnsError verifies that a malformed
// page_token is rejected.
func TestListMessagesPaged_InvalidCursor_ReturnsError(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	_, _, err := store.ListMessagesPaged(context.Background(), "aaaaaaaa-0000-7000-8000-000000000006", "not-a-token", 10, "asc")
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

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc")
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

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc")
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

	paged, _, err := store.ListMessagesPaged(context.Background(), sessionID, "", 10, "asc")
	require.NoError(t, err)
	require.Len(t, paged, 1)
	assert.Equal(t, "w-abc123", paged[0].AgentInstanceID)

	var nullInstances int
	require.NoError(t, store.db.GetContext(context.Background(), &nullInstances,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND agent_instance_id IS NULL`, sessionID))
	assert.Equal(t, 0, nullInstances)
}
