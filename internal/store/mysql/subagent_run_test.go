package mysql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

func setupSubAgentRunStore(t *testing.T) (*Store, func()) {
	t.Helper()
	if testDSN() == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed subagent snapshot test")
	}
	store, err := New(testDSN())
	require.NoError(t, err)
	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS subagent_runs")
		_ = store.Close()
	}
	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE subagent_runs (
			id BIGINT NOT NULL AUTO_INCREMENT,
			session_id CHAR(36) NOT NULL,
			agent_instance_id CHAR(24) NOT NULL,
			parent_instance_id CHAR(24) NULL,
			depth INT NOT NULL,
			name VARCHAR(128) NOT NULL,
			tools_json JSON NULL,
			status VARCHAR(16) NOT NULL,
			resume_eligible TINYINT(1) NOT NULL DEFAULT 1,
			messages_json JSON NOT NULL,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_subagent_runs_session_instance (session_id, agent_instance_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	return store, cleanup
}

func TestSubAgentRuns_UpsertIdempotentAndScopedRead(t *testing.T) {
	store, cleanup := setupSubAgentRunStore(t)
	defer cleanup()
	ctx := context.Background()

	run := model.SubAgentRun{
		SessionID: "aaaaaaaa-0000-7000-8000-0000000000ba", AgentInstanceID: "w-abc123",
		ParentInstanceID: "w-parent", Depth: 2, Name: "writer#abc123",
		ToolsJSON: []byte(`["read"]`), Status: model.SubAgentStatusCompleted,
		ResumeEligible: true, MessagesJSON: []byte(`[{"role":"user","content":"hello"}]`),
	}
	require.NoError(t, store.UpsertSubAgentRun(ctx, run))
	run.Status = model.SubAgentStatusCapped
	run.ResumeEligible = false
	run.MessagesJSON = []byte(`[{"role":"user","content":"hello"},{"role":"assistant","content":"partial"}]`)
	require.NoError(t, store.UpsertSubAgentRun(ctx, run))

	got, ok, err := store.GetSubAgentRun(ctx, run.SessionID, run.AgentInstanceID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, model.SubAgentStatusCapped, got.Status)
	assert.False(t, got.ResumeEligible)
	assert.Equal(t, run.ParentInstanceID, got.ParentInstanceID)

	_, ok, err = store.GetSubAgentRun(ctx, "aaaaaaaa-0000-7000-8000-0000000000bb", run.AgentInstanceID)
	require.NoError(t, err)
	assert.False(t, ok, "a session-scoped lookup must not see another session's instance")
}

func TestSubAgentRuns_SessionDeleteCascades(t *testing.T) {
	if testDSN() == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed cascade test")
	}
	store, err := New(testDSN())
	require.NoError(t, err)
	ctx := context.Background()
	defer func() {
		_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS subagent_runs")
		_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS sessions_cascade_test")
		_ = store.Close()
	}()
	_, err = store.db.ExecContext(ctx, `
		CREATE TABLE sessions_cascade_test (
			session_id CHAR(36) PRIMARY KEY,
			user_id VARCHAR(64) NOT NULL,
			trace_id CHAR(36) NOT NULL,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `
		CREATE TABLE subagent_runs (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			session_id CHAR(36) NOT NULL,
			agent_instance_id CHAR(24) NOT NULL,
			parent_instance_id CHAR(24) NULL,
			depth INT NOT NULL,
			name VARCHAR(128) NOT NULL,
			tools_json JSON NULL,
			status VARCHAR(16) NOT NULL,
			resume_eligible TINYINT(1) NOT NULL,
			messages_json JSON NOT NULL,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uk_subagent_runs_session_instance (session_id, agent_instance_id),
			CONSTRAINT fk_subagent_runs_session_test FOREIGN KEY (session_id)
				REFERENCES sessions_cascade_test(session_id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)
	sessionID := "aaaaaaaa-0000-7000-8000-0000000000bc"
	_, err = store.db.ExecContext(ctx, `INSERT INTO sessions_cascade_test VALUES (?, 'u', 't', NOW(), NOW())`, sessionID)
	require.NoError(t, err)
	require.NoError(t, store.UpsertSubAgentRun(ctx, model.SubAgentRun{
		SessionID: sessionID, AgentInstanceID: "w-cascade", Depth: 1, Name: "x",
		Status: model.SubAgentStatusCompleted, ResumeEligible: true, MessagesJSON: []byte(`[]`),
	}))
	_, err = store.db.ExecContext(ctx, "DELETE FROM sessions_cascade_test WHERE session_id = ?", sessionID)
	require.NoError(t, err)
	_, ok, err := store.GetSubAgentRun(ctx, sessionID, "w-cascade")
	require.NoError(t, err)
	assert.False(t, ok)
}
