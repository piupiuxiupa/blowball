package mysql

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// not require a sessions row. The column layout matches production.
	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS messages (
			id          BIGINT       NOT NULL AUTO_INCREMENT,
			session_id  CHAR(36)     NOT NULL,
			msg_time    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			agent       VARCHAR(32)  NOT NULL,
			msg_index   INT          NOT NULL,
			role        VARCHAR(16)  NOT NULL,
			content     MEDIUMTEXT   NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			KEY idx_messages_session_time (session_id, msg_time)
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
// cursor advances to the next page.
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
	assert.Empty(t, next2, "last page must have empty next_page_token")
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
