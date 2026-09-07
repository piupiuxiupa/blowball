package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

func setupSubAgentRunStore(t *testing.T) (*Store, func()) {
	t.Helper()
	if testDSN() == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed subagent run test")
	}
	store, err := New(testDSN())
	require.NoError(t, err)
	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS subagent_runs")
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS subagent_instances")
		_ = store.Close()
	}
	_, err = store.db.ExecContext(context.Background(), `
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

func testSubAgentInstance(sessionID, instanceID string) model.SubAgentInstance {
	return model.SubAgentInstance{
		SessionID: sessionID, AgentInstanceID: instanceID,
		ParentInstanceID: "w-parent", Depth: 2, Name: "writer#" + instanceID,
		SystemPrompt: "be useful", ToolsJSON: []byte(`["read"]`),
	}
}

func testSubAgentRun(instance model.SubAgentInstance, runID, previous string, runNo int) model.SubAgentRun {
	return model.SubAgentRun{
		SessionID: instance.SessionID, AgentInstanceID: instance.AgentInstanceID,
		RunID: runID, PreviousRunID: previous, RunNo: runNo,
		ParentInstanceID: instance.ParentInstanceID, Depth: instance.Depth,
		Name: instance.Name, ToolsJSON: instance.ToolsJSON,
		Status: model.SubAgentStatusCompleted, SnapshotKind: model.SubAgentSnapshotDelta,
		ResumeEligible: true, BaseMessageCount: runNo - 1, MessageCount: 2,
		ContextMessageCount: runNo + 1, ContextBytes: 100,
		MessagesJSON: []byte(`[{"role":"user","content":"hello"}]`),
		StartedAt:    time.Now().UTC(), FinishedAt: time.Now().UTC().Add(time.Second),
	}
}

func TestSubAgentRuns_MultipleDeltasRetryAndScopedRead(t *testing.T) {
	store, cleanup := setupSubAgentRunStore(t)
	defer cleanup()
	ctx := context.Background()
	sessionID := "aaaaaaaa-0000-7000-8000-0000000000ba"
	instance := testSubAgentInstance(sessionID, "w-abc123")
	run1 := testSubAgentRun(instance, "call-1", "", 1)
	run2 := testSubAgentRun(instance, "call-2", "call-1", 2)

	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run1))
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run2))

	run2.Status = model.SubAgentStatusError
	run2.MessageCount = 3
	run2.MessagesJSON = []byte(`[{"role":"user","content":"hello"},{"role":"assistant","content":"partial"}]`)
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run2))

	got, ok, err := store.GetSubAgentHistory(ctx, sessionID, instance.AgentInstanceID)
	require.NoError(t, err)
	assert.True(t, ok)
	require.Len(t, got.Runs, 2)
	assert.Equal(t, "call-1", got.Runs[0].RunID)
	assert.Equal(t, "call-2", got.Runs[1].RunID)
	assert.Equal(t, model.SubAgentStatusError, got.Runs[1].Status)
	assert.Equal(t, "call-2", got.Instance.LatestRunID)

	exact, ok, err := store.GetSubAgentRun(ctx, sessionID, instance.AgentInstanceID, "call-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, exact.RunNo)

	_, ok, err = store.GetSubAgentRun(ctx, sessionID, instance.AgentInstanceID, "missing")
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = store.GetSubAgentInstance(ctx, "aaaaaaaa-0000-7000-8000-0000000000bb", instance.AgentInstanceID)
	require.NoError(t, err)
	assert.False(t, ok, "lookups are session-scoped")
}

func TestSubAgentRuns_ConcurrentResumeCannotForkChain(t *testing.T) {
	store, cleanup := setupSubAgentRunStore(t)
	defer cleanup()
	ctx := context.Background()
	instance := testSubAgentInstance("aaaaaaaa-0000-7000-8000-0000000000bd", "w-race")
	run1 := testSubAgentRun(instance, "call-1", "", 1)
	run2a := testSubAgentRun(instance, "call-2a", "call-1", 2)
	run3 := testSubAgentRun(instance, "call-3", "call-2a", 3)

	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run1))
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run2a))
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, run3))

	stale := testSubAgentRun(instance, "call-2b", "call-1", 2)
	err := store.AppendSubAgentRun(ctx, instance, stale)
	assert.ErrorIs(t, err, model.ErrSubAgentChainConflict)

	got, ok, err := store.GetSubAgentHistory(ctx, instance.SessionID, instance.AgentInstanceID)
	require.NoError(t, err)
	assert.True(t, ok)
	require.Len(t, got.Runs, 3)
	assert.Equal(t, "call-3", got.Instance.LatestRunID)
}

func TestSubAgentRuns_RunIdentityCollisionRejected(t *testing.T) {
	store, cleanup := setupSubAgentRunStore(t)
	defer cleanup()
	ctx := context.Background()
	instance := testSubAgentInstance("aaaaaaaa-0000-7000-8000-0000000000be", "w-one")
	other := instance
	other.AgentInstanceID = "w-two"
	require.NoError(t, store.AppendSubAgentRun(ctx, instance, testSubAgentRun(instance, "shared-call", "", 1)))

	err := store.AppendSubAgentRun(ctx, other, testSubAgentRun(other, "shared-call", "", 1))
	assert.Error(t, err)
	_, ok, err := store.GetSubAgentInstance(ctx, other.SessionID, other.AgentInstanceID)
	require.NoError(t, err)
	assert.False(t, ok, "the collision attempt must roll back instance creation")
}
