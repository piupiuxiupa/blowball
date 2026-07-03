package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupArchiveTestStore opens a connection and creates the live sessions /
// titles / messages tables plus their *_deleted mirrors, matching the
// production layout. titles and messages carry ON DELETE CASCADE FKs to
// sessions so the cascade purge is exercised exactly as in production; the
// sessions.user_id FK to users is dropped so tests need no users row. Tests are
// skipped when MYSQL_TEST_DSN is not set.
func setupArchiveTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}

	store, err := New(dsn)
	require.NoError(t, err)

	ddl := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			session_id  CHAR(36)  NOT NULL,
			user_id     CHAR(36)  NOT NULL,
			trace_id    CHAR(36)  NOT NULL,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS titles (
			session_id  CHAR(36)     NOT NULL,
			title       VARCHAR(128) NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			create_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id),
			CONSTRAINT fk_titles_session FOREIGN KEY (session_id) REFERENCES sessions (session_id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS messages (
			id          BIGINT       NOT NULL AUTO_INCREMENT,
			session_id  CHAR(36)     NOT NULL,
			msg_time    TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			agent       VARCHAR(32)  NOT NULL,
			msg_index   INT          NOT NULL,
			role        VARCHAR(16)  NULL,
			event_type  VARCHAR(16)  NOT NULL DEFAULT 'token',
			content     MEDIUMTEXT   NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			CONSTRAINT fk_messages_session FOREIGN KEY (session_id) REFERENCES sessions (session_id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS sessions_deleted (
			session_id  CHAR(36)  NOT NULL,
			user_id     CHAR(36)  NOT NULL,
			trace_id    CHAR(36)  NOT NULL,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deletion_id CHAR(36)  NOT NULL,
			PRIMARY KEY (session_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS titles_deleted (
			session_id  CHAR(36)     NOT NULL,
			title       VARCHAR(128) NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			create_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deletion_id CHAR(36)     NOT NULL,
			PRIMARY KEY (session_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS messages_deleted (
			id          BIGINT       NOT NULL,
			session_id  CHAR(36)     NOT NULL,
			msg_time    TIMESTAMP(3) NOT NULL,
			agent       VARCHAR(32)  NOT NULL,
			msg_index   INT          NOT NULL,
			role        VARCHAR(16)  NULL,
			event_type  VARCHAR(16)  NOT NULL,
			content     MEDIUMTEXT   NOT NULL,
			trace_id    CHAR(36)     NOT NULL,
			update_time TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deletion_id CHAR(36)     NOT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, q := range ddl {
		_, err := store.db.ExecContext(context.Background(), q)
		require.NoError(t, err)
	}

	cleanup := func() {
		ctx := context.Background()
		for _, tbl := range []string{"messages_deleted", "titles_deleted", "sessions_deleted", "messages", "titles", "sessions"} {
			_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
		}
		_ = store.Close()
	}
	return store, cleanup
}

// seedSession inserts a session row, an optional title, and count message rows,
// returning the message ids. Used to set up a deletable session.
func seedSession(t *testing.T, s *Store, sessionID, userID string, title string, msgCount int) []int64 {
	t.Helper()
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (session_id, user_id, trace_id) VALUES (?, ?, 'trace-seed')",
		sessionID, userID)
	require.NoError(t, err)
	if title != "" {
		_, err = s.db.ExecContext(ctx,
			"INSERT INTO titles (session_id, title, trace_id) VALUES (?, ?, 'trace-seed')",
			sessionID, title)
		require.NoError(t, err)
	}
	ids := make([]int64, 0, msgCount)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < msgCount; i++ {
		res, err := s.db.ExecContext(ctx,
			"INSERT INTO messages (session_id, msg_time, agent, msg_index, role, event_type, content, trace_id) VALUES (?, ?, 'user', ?, 'user', 'message', ?, 'trace-seed')",
			sessionID, base.Add(time.Duration(i)*time.Second), i, "body")
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

// archivedSession mirrors the columns tests read back from sessions_deleted.
type archivedSession struct {
	SessionID  string    `db:"session_id"`
	UserID     string    `db:"user_id"`
	DeletionID string    `db:"deletion_id"`
	DeletedAt  time.Time `db:"deleted_at"`
}

// archivedMessage mirrors the columns tests read back from messages_deleted.
type archivedMessage struct {
	ID         int64     `db:"id"`
	SessionID  string    `db:"session_id"`
	Content    string    `db:"content"`
	DeletionID string    `db:"deletion_id"`
}

// TestDeleteSession_ArchivesAndPurges verifies that deleting a session copies the
// session, its title and its messages verbatim into the *_deleted mirrors
// (sharing one deletion_id), and that the live rows are purged via cascade.
func TestDeleteSession_ArchivesAndPurges(t *testing.T) {
	store, cleanup := setupArchiveTestStore(t)
	defer cleanup()

	const (
		sessionID = "bbbbbbbb-0000-7000-8000-000000000001"
		userID    = "bbbbbbbb-0000-7000-8000-000000000099"
	)
	msgIDs := seedSession(t, store, sessionID, userID, "My Title", 2)
	ctx := context.Background()

	require.NoError(t, store.DeleteSession(ctx, sessionID))

	// sessions_deleted: exactly one row for the session with a non-empty
	// deletion_id.
	var arch archivedSession
	require.NoError(t, store.db.GetContext(ctx, &arch,
		"SELECT session_id, user_id, deletion_id, deleted_at FROM sessions_deleted WHERE session_id = ?",
		sessionID))
	assert.Equal(t, userID, arch.UserID)
	require.NotEmpty(t, arch.DeletionID)

	// All mirrored rows from this delete share the same deletion_id.
	var sharedDeletion string
	require.NoError(t, store.db.GetContext(ctx, &sharedDeletion,
		"SELECT deletion_id FROM titles_deleted WHERE session_id = ?", sessionID))
	assert.Equal(t, arch.DeletionID, sharedDeletion, "title must share the session's deletion_id")

	// Every mirror table from this delete shares the same deleted_at too (the
	// spec's "same deletion_id AND same deleted_at" requirement). deleted_at is
	// bound once in Go and passed to all three INSERTs, so it cannot diverge
	// across a wall-clock second boundary the way per-statement NOW() would.
	var titleDeletedAt time.Time
	require.NoError(t, store.db.GetContext(ctx, &titleDeletedAt,
		"SELECT deleted_at FROM titles_deleted WHERE session_id = ?", sessionID))
	var msgDeletedAt time.Time
	require.NoError(t, store.db.GetContext(ctx, &msgDeletedAt,
		"SELECT deleted_at FROM messages_deleted WHERE session_id = ? LIMIT 1", sessionID))
	assert.Equal(t, arch.DeletedAt, titleDeletedAt, "titles_deleted.deleted_at must match sessions_deleted")
	assert.Equal(t, arch.DeletedAt, msgDeletedAt, "messages_deleted.deleted_at must match sessions_deleted")

	var archMsgs []archivedMessage
	require.NoError(t, store.db.SelectContext(ctx, &archMsgs,
		"SELECT id, session_id, content, deletion_id FROM messages_deleted WHERE session_id = ? ORDER BY id",
		sessionID))
	require.Len(t, archMsgs, 2)
	// Original message ids are preserved verbatim.
	assert.Equal(t, msgIDs, []int64{archMsgs[0].ID, archMsgs[1].ID})
	for _, m := range archMsgs {
		assert.Equal(t, arch.DeletionID, m.DeletionID, "every message must share the deletion_id")
		assert.Equal(t, "body", m.Content)
	}

	// Live rows are gone: session purged, cascade cleared titles and messages.
	var liveSess int
	require.NoError(t, store.db.GetContext(ctx, &liveSess,
		"SELECT COUNT(*) FROM sessions WHERE session_id = ?", sessionID))
	assert.Equal(t, 0, liveSess)
	var liveMsgs int
	require.NoError(t, store.db.GetContext(ctx, &liveMsgs,
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID))
	assert.Equal(t, 0, liveMsgs, "messages must be cascade-purged")
	var liveTitles int
	require.NoError(t, store.db.GetContext(ctx, &liveTitles,
		"SELECT COUNT(*) FROM titles WHERE session_id = ?", sessionID))
	assert.Equal(t, 0, liveTitles, "title must be cascade-purged")
}

// TestDeleteSession_Idempotent verifies deleting a non-existent session returns
// nil without writing any archive rows.
func TestDeleteSession_Idempotent(t *testing.T) {
	store, cleanup := setupArchiveTestStore(t)
	defer cleanup()

	const sessionID = "bbbbbbbb-0000-7000-8000-000000000002"
	ctx := context.Background()

	require.NoError(t, store.DeleteSession(ctx, sessionID),
		"deleting a missing session must be a no-op nil")

	var n int
	require.NoError(t, store.db.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM sessions_deleted WHERE session_id = ?", sessionID))
	assert.Equal(t, 0, n, "no archive row for a session that never existed")
}

// TestDeleteSession_ArchiveFailure_RollsBack verifies that when an archive step
// fails mid-transaction, the whole delete rolls back: live data stays intact
// and no partial archive survives. We force the failure by dropping the
// messages_deleted mirror after seeding, so archiveMessagesSQL errors.
func TestDeleteSession_ArchiveFailure_RollsBack(t *testing.T) {
	store, cleanup := setupArchiveTestStore(t)
	defer cleanup()

	const (
		sessionID = "bbbbbbbb-0000-7000-8000-000000000003"
		userID    = "bbbbbbbb-0000-7000-8000-000000000098"
	)
	seedSession(t, store, sessionID, userID, "Keep Me", 1)
	ctx := context.Background()

	// Remove the messages mirror so the third archive statement fails; the two
	// preceding archives (sessions, titles) must be rolled back with it.
	_, err := store.db.ExecContext(ctx, "DROP TABLE messages_deleted")
	require.NoError(t, err)

	err = store.DeleteSession(ctx, sessionID)
	require.Error(t, err, "archive failure must surface as an error")

	// Live data is intact.
	var liveSess int
	require.NoError(t, store.db.GetContext(ctx, &liveSess,
		"SELECT COUNT(*) FROM sessions WHERE session_id = ?", sessionID))
	assert.Equal(t, 1, liveSess, "live session must survive a rolled-back delete")
	var liveMsgs int
	require.NoError(t, store.db.GetContext(ctx, &liveMsgs,
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID))
	assert.Equal(t, 1, liveMsgs, "live messages must survive a rolled-back delete")

	// No partial archive: the sessions archive written earlier in the tx is gone.
	var archived int
	require.NoError(t, store.db.GetContext(ctx, &archived,
		"SELECT COUNT(*) FROM sessions_deleted WHERE session_id = ?", sessionID))
	assert.Equal(t, 0, archived, "rolled-back tx must leave no archive rows")
}
