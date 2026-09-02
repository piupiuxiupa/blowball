package mysql

import (
	"context"
	"testing"

	"github.com/lush/blowball/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTurnUsageTestStore opens a connection and creates the minimal
// sessions + turn_usage layout (migration 010 + 013 + 015): the model column
// is nullable and context_tokens records the end-of-turn context size. The
// sessions.user_id FK is dropped so tests need no users row. Tests are
// skipped when MYSQL_TEST_DSN is not set.
func setupTurnUsageTestStore(t *testing.T) (*Store, func()) {
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
		`CREATE TABLE IF NOT EXISTS turn_usage (
			id             BIGINT       NOT NULL AUTO_INCREMENT,
			session_id     CHAR(36)     NOT NULL,
			trace_id       CHAR(36)     NOT NULL,
			user_id        CHAR(36)     NOT NULL,
			usage_json     JSON         NOT NULL,
			total_tokens   INT          NOT NULL DEFAULT 0,
			model          VARCHAR(64)  NULL,
			context_tokens INT          NOT NULL DEFAULT 0,
			create_time    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			CONSTRAINT fk_turn_usage_session FOREIGN KEY (session_id) REFERENCES sessions (session_id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, stmt := range ddl {
		_, err := store.db.Exec(stmt)
		require.NoError(t, err)
	}
	cleanup := func() {
		_, _ = store.db.Exec(`DROP TABLE IF EXISTS turn_usage, sessions`)
		_ = store.db.Close()
	}
	return store, cleanup
}

// TestSaveTurnUsage_ModelColumn (migration 015, per-request-model-selection)
// verifies the model dimension round-trips: a turn that resolved a model name
// persists it verbatim, and an empty Go string stores NULL (identical to
// pre-migration rows) rather than an empty string.
func TestSaveTurnUsage_ModelColumn(t *testing.T) {
	store, cleanup := setupTurnUsageTestStore(t)
	defer cleanup()

	ctx := context.Background()
	const sessionID = "turn-usage-model-test-sess"
	const userID = "user-1"
	_, err := store.db.Exec(`INSERT INTO sessions (session_id, user_id, trace_id) VALUES (?, ?, ?)`,
		sessionID, userID, "trace-0")
	require.NoError(t, err)
	defer store.db.Exec(`DELETE FROM sessions WHERE session_id = ?`, sessionID)

	base := func(modelName string, trace string) model.TurnUsage {
		return model.TurnUsage{
			SessionID:     sessionID,
			TraceID:       trace,
			UserID:        userID,
			Model:         modelName,
			UsageJSON:     `{"total":{"total_tokens":42}}`,
			TotalTokens:   42,
			ContextTokens: 37,
		}
	}

	// A resolved model name round-trips verbatim.
	require.NoError(t, store.SaveTurnUsage(ctx, base("glm-4.7", "trace-1")))

	// An empty model stores NULL (NULLIF), never ''.
	require.NoError(t, store.SaveTurnUsage(ctx, base("", "trace-2")))

	rows, err := store.db.Queryx(
		`SELECT trace_id, model, model IS NULL AS model_is_null FROM turn_usage WHERE session_id = ? ORDER BY id`, sessionID)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]struct {
		model  *string
		isNull bool
	}{}
	for rows.Next() {
		var traceID string
		var modelPtr *string
		var isNull bool
		require.NoError(t, rows.Scan(&traceID, &modelPtr, &isNull))
		got[traceID] = struct {
			model  *string
			isNull bool
		}{modelPtr, isNull}
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2)

	sel := got["trace-1"]
	require.NotNil(t, sel.model, "resolved model must be stored, not NULL")
	assert.Equal(t, "glm-4.7", *sel.model)
	assert.False(t, sel.isNull)

	empty := got["trace-2"]
	assert.Nil(t, empty.model, "empty model string must persist as NULL")
	assert.True(t, empty.isNull)
}
