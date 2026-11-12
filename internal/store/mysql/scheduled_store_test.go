package mysql

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// scheduledSchemaFiles are the production migrations that define the task
// table group and its execution-control tables. Applying the real files in
// tests keeps fixtures and production schema from drifting apart.
var scheduledSchemaFiles = []string{
	"../../../migrations/028_scheduled_session_store.sql",
	"../../../migrations/029_scheduled_execution_control.sql",
	"../../../migrations/031_scheduled_subagent_run_timestamps.sql",
}

var scheduledTableNames = []string{
	"scheduled_attachments_deleted", "scheduled_attachments",
	"scheduled_titles_deleted", "scheduled_titles",
	"scheduled_turn_usage", "scheduled_context_compactions",
	"scheduled_subagent_runs", "scheduled_subagent_instances",
	"scheduled_tool_calls", "scheduled_messages_deleted", "scheduled_messages",
	"scheduled_sessions_deleted", "scheduled_sessions",
	"scheduled_execution_receipts", "scheduled_resource_drafts", "scheduled_resource_versions",
}

func setupScheduledStore(t *testing.T) (*Store, *SessionStore, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed store test")
	}
	store, err := New(dsn)
	require.NoError(t, err)
	lockTestSchema(t, store)
	ctx := context.Background()

	if _, err := store.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		require.NoError(t, err)
	}
	for _, tbl := range scheduledTableNames {
		_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS sessions")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS messages")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS tool_calls")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS titles")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS turn_usage")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS attachments")
	_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS attachments_deleted")
	if _, err := store.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1"); err != nil {
		require.NoError(t, err)
	}

	for _, file := range scheduledSchemaFiles {
		execSQLFile(t, store, file)
	}

	// Minimal ordinary tables used only to prove scheduled writes never touch
	// them.
	for _, q := range []string{
		`CREATE TABLE sessions (
			session_id CHAR(36) NOT NULL PRIMARY KEY,
			user_id CHAR(36) NOT NULL,
			trace_id CHAR(36) NOT NULL,
			context_compacted TINYINT(1) NOT NULL DEFAULT 0,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE messages (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			session_id CHAR(36) NOT NULL,
			msg_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			agent VARCHAR(32) NOT NULL,
			msg_index INT NOT NULL,
			role VARCHAR(16) NULL,
			event_type VARCHAR(16) NOT NULL DEFAULT 'token',
			content MEDIUMTEXT NOT NULL,
			trace_id CHAR(36) NOT NULL,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE tool_calls (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			session_id CHAR(36) NOT NULL,
			turn_id CHAR(36) NOT NULL,
			tool_call_id VARCHAR(64) NOT NULL,
			name VARCHAR(128) NOT NULL,
			arguments MEDIUMTEXT NOT NULL,
			result MEDIUMTEXT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			update_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
			UNIQUE KEY uk_tool_calls_session_call (session_id, tool_call_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE titles (
			session_id CHAR(36) NOT NULL PRIMARY KEY,
			title VARCHAR(128) NOT NULL,
			trace_id CHAR(36) NOT NULL,
			is_manual TINYINT(1) NOT NULL DEFAULT 0,
			update_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			create_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE turn_usage (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			session_id CHAR(36) NOT NULL,
			trace_id CHAR(36) NOT NULL,
			user_id CHAR(36) NOT NULL,
			usage_json MEDIUMTEXT NOT NULL,
			total_tokens INT NOT NULL,
			model VARCHAR(64) NULL,
			context_tokens INT NOT NULL DEFAULT 0,
			created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE attachments (
			id CHAR(36) NOT NULL PRIMARY KEY,
			user_id CHAR(36) NOT NULL,
			session_id CHAR(36) NULL,
			filename VARCHAR(255) NOT NULL,
			mime VARCHAR(64) NOT NULL,
			size BIGINT NOT NULL,
			message_id BIGINT NULL,
			create_time TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		_, err := store.db.ExecContext(ctx, q)
		require.NoError(t, err)
	}

	cleanup := func() {
		_, _ = store.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0")
		for _, tbl := range scheduledTableNames {
			_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
		}
		for _, tbl := range []string{"sessions", "messages", "tool_calls", "titles", "turn_usage", "attachments"} {
			_, _ = store.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
		}
		_, _ = store.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
		_ = store.Close()
	}
	return store, store.ForSource(model.SessionSourceScheduled), cleanup
}

// execSQLFile applies a migration file statement by statement, skipping
// comment lines (which may themselves contain semicolons) and accumulating
// lines until one ends with a semicolon. The migration files contain no
// semicolons inside string literals, so this is sufficient.
func execSQLFile(t *testing.T, store *Store, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var stmt strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if _, err := store.db.ExecContext(context.Background(), stmt.String()); err != nil {
				t.Fatalf("apply %s: %v\nstatement: %s", path, err, stmt.String())
			}
			stmt.Reset()
		}
	}
	if strings.TrimSpace(stmt.String()) != "" {
		t.Fatalf("apply %s: trailing statement without semicolon: %s", path, stmt.String())
	}
}

func scheduledTestSession(sessionID, occurrenceID, runID string) model.Session {
	return model.Session{
		SessionID:    sessionID,
		UserID:       "user-1",
		TaskID:       "task-1",
		OccurrenceID: occurrenceID,
		RunID:        runID,
		TraceID:      "trace-1",
	}
}

func TestScheduledSessionCreateIsIdempotent(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	sess := scheduledTestSession("ss-1", "occ-1", "run-1")
	first, err := scheduled.CreateScheduledSession(ctx, sess)
	require.NoError(t, err)
	second, err := scheduled.CreateScheduledSession(ctx, sess)
	require.NoError(t, err)
	assert.Equal(t, first.SessionID, second.SessionID)
	assert.Equal(t, model.SessionSourceScheduled, second.Source)

	var count int
	require.NoError(t, store.db.GetContext(ctx, &count, "SELECT COUNT(*) FROM scheduled_sessions"))
	assert.Equal(t, 1, count)

	// Same session identity but a different occurrence identity is a hard
	// conflict, never a second session or a silent rewrite.
	conflict := sess
	conflict.OccurrenceID = "occ-2"
	_, err = scheduled.CreateScheduledSession(ctx, conflict)
	assert.ErrorIs(t, err, ErrScheduledSessionConflict)
}

func TestSessionSourceViewsDoNotFallBack(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := scheduled.CreateScheduledSession(ctx, scheduledTestSession("ss-2", "occ-2", "run-2"))
	require.NoError(t, err)

	interactive := store.ForSource(model.SessionSourceInteractive)
	got, err := interactive.GetSessionByID(ctx, "ss-2")
	require.NoError(t, err)
	assert.Nil(t, got, "interactive view must not see a scheduled session")

	// The converse: an ordinary session is invisible to the scheduled view.
	_, err = store.db.ExecContext(ctx, "INSERT INTO sessions (session_id, user_id, trace_id) VALUES ('os-1','user-1','t')")
	require.NoError(t, err)
	got, err = scheduled.GetScheduledSessionByID(ctx, "os-1")
	require.NoError(t, err)
	assert.Nil(t, got, "scheduled view must not see an ordinary session")
}

func TestInteractiveCreateRejectsScheduledIdentity(t *testing.T) {
	store, _, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	interactive := store.ForSource(model.SessionSourceInteractive)
	err := interactive.CreateSession(ctx, scheduledTestSession("ss-3", "occ-3", "run-3"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot carry task identity")
}

func TestScheduledWritesStayInScheduledTables(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	sess := scheduledTestSession("ss-4", "occ-4", "run-4")
	_, err := scheduled.CreateScheduledSession(ctx, sess)
	require.NoError(t, err)

	msgs := []model.Message{
		{SessionID: sess.SessionID, MsgTime: time.Unix(1_700_000_000, 0).UTC(), Agent: model.AgentUser, MsgIndex: 0, Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hi", TraceID: "trace-1", ClientMsgID: "trace-1:0"},
		{SessionID: sess.SessionID, MsgTime: time.Unix(1_700_000_001, 0).UTC(), Agent: model.AgentConfucius, MsgIndex: 1, Role: model.RoleAssistant, EventType: model.EventTypeMessage, Content: "hello", TraceID: "trace-1", ClientMsgID: "trace-1:1"},
	}
	calls := []model.ToolCall{
		{SessionID: sess.SessionID, TurnID: "trace-1", ToolCallID: "call-1", Name: "xizhi_read_file", Arguments: "{}", Result: "ok", Status: "completed"},
		{SessionID: sess.SessionID, TurnID: "trace-1", ToolCallID: "call-2", Name: "xizhi_list_files", Arguments: "{}", Result: "ok", Status: "completed"},
		{SessionID: sess.SessionID, TurnID: "trace-1", ToolCallID: "call-3", Name: "xizhi_grep", Arguments: "{}", Result: "ok", Status: "completed"},
	}
	require.NoError(t, scheduled.AppendFinalBatch(ctx, msgs, calls))
	require.NoError(t, scheduled.UpsertTitle(ctx, model.Title{SessionID: sess.SessionID, Title: "t", TraceID: "trace-1"}))
	require.NoError(t, scheduled.SaveTurnUsage(ctx, model.TurnUsage{SessionID: sess.SessionID, TraceID: "trace-1", UserID: "user-1", UsageJSON: "{}", TotalTokens: 3, ContextTokens: 2}))
	require.NoError(t, scheduled.InsertCompaction(ctx, model.ContextCompaction{
		SessionID: sess.SessionID, UserID: "user-1", TraceID: "trace-1", TriggerKind: "mid_turn",
		TriggerTokens: 10, Content: "summary", BoundaryMsgTime: time.Unix(1_700_000_001, 0).UTC(),
		BoundaryMsgIndex: 1, BoundaryMsgID: 2, ShadowedTokens: 5, SummaryModel: "m",
	}))
	instance := model.SubAgentInstance{SessionID: sess.SessionID, AgentInstanceID: "w-1", Depth: 1, Name: "w", SystemPrompt: "s"}
	require.NoError(t, scheduled.AppendSubAgentRun(ctx, instance, model.SubAgentRun{
		SessionID: sess.SessionID, AgentInstanceID: "w-1", RunID: "run-child", RunNo: 1,
		Depth: 1, Name: "w", Status: "completed", SnapshotKind: "delta", ResumeEligible: true,
		MessagesJSON: []byte(`[]`), StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	}))
	attachmentSession := sess.SessionID
	require.NoError(t, scheduled.InsertAttachment(ctx, model.Attachment{
		ID: "att-1", UserID: "user-1", SessionID: &attachmentSession,
		Filename: "a.png", MIME: "image/png", Size: 1,
	}))

	var scheduledMsgs, ordinaryMsgs, ordinaryCalls, ordinaryTitles, ordinaryUsage, ordinaryAttachments int
	require.NoError(t, store.db.GetContext(ctx, &scheduledMsgs, "SELECT COUNT(*) FROM scheduled_messages"))
	require.NoError(t, store.db.GetContext(ctx, &ordinaryMsgs, "SELECT COUNT(*) FROM messages"))
	require.NoError(t, store.db.GetContext(ctx, &ordinaryCalls, "SELECT COUNT(*) FROM tool_calls"))
	require.NoError(t, store.db.GetContext(ctx, &ordinaryTitles, "SELECT COUNT(*) FROM titles"))
	require.NoError(t, store.db.GetContext(ctx, &ordinaryUsage, "SELECT COUNT(*) FROM turn_usage"))
	require.NoError(t, store.db.GetContext(ctx, &ordinaryAttachments, "SELECT COUNT(*) FROM attachments"))
	assert.Equal(t, 2, scheduledMsgs)
	assert.Zero(t, ordinaryMsgs)
	assert.Zero(t, ordinaryCalls)
	assert.Zero(t, ordinaryTitles)
	assert.Zero(t, ordinaryUsage)
	assert.Zero(t, ordinaryAttachments)

	// Reading back through the scheduled view returns the rows.
	recovered, err := scheduled.ListMessages(ctx, sess.SessionID)
	require.NoError(t, err)
	assert.Len(t, recovered, 2)
	gotCalls, err := scheduled.ListToolCalls(ctx, sess.SessionID)
	require.NoError(t, err)
	require.Len(t, gotCalls, 3)
	assert.Equal(t, []string{"call-1", "call-2", "call-3"}, []string{gotCalls[0].ToolCallID, gotCalls[1].ToolCallID, gotCalls[2].ToolCallID},
		"tool interactions keep response order")

	// No chunk/event rows are produced by the final-batch path: the scheduled
	// message table holds only finals and structural rows.
	var chunkRows int
	require.NoError(t, store.db.GetContext(ctx, &chunkRows,
		"SELECT COUNT(*) FROM scheduled_messages WHERE event_type IN ('token','reasoning','tool_result','agent_start','agent_end')"))
	assert.Zero(t, chunkRows, "new persistence writes finals, never the retired chunk stream")
}

func TestScheduledDeleteArchivesAndCascades(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	sess := scheduledTestSession("ss-5", "occ-5", "run-5")
	_, err := scheduled.CreateScheduledSession(ctx, sess)
	require.NoError(t, err)
	require.NoError(t, scheduled.AppendFinalBatch(ctx, []model.Message{{
		SessionID: sess.SessionID, MsgTime: time.Now().UTC(), Agent: model.AgentUser, MsgIndex: 0,
		Role: model.RoleUser, EventType: model.EventTypeMessage, Content: "hi", TraceID: "trace-1",
	}}, nil))

	require.NoError(t, scheduled.DeleteSession(ctx, sess.SessionID))

	var archived, live int
	require.NoError(t, store.db.GetContext(ctx, &archived, "SELECT COUNT(*) FROM scheduled_messages_deleted"))
	require.NoError(t, store.db.GetContext(ctx, &live, "SELECT COUNT(*) FROM scheduled_messages"))
	assert.Equal(t, 1, archived)
	assert.Zero(t, live, "cascading delete clears the live child rows")
	var sessionArchived int
	require.NoError(t, store.db.GetContext(ctx, &sessionArchived, "SELECT COUNT(*) FROM scheduled_sessions_deleted WHERE task_id='task-1'"))
	assert.Equal(t, 1, sessionArchived)
}

func TestListScheduledSessionsOwnerScoped(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := scheduled.CreateScheduledSession(ctx, scheduledTestSession("ss-6", "occ-6", "run-6"))
	require.NoError(t, err)
	other := scheduledTestSession("ss-7", "occ-7", "run-7")
	other.UserID = "user-2"
	other.TaskID = "task-2"
	_, err = scheduled.CreateScheduledSession(ctx, other)
	require.NoError(t, err)

	rows, err := scheduled.ListScheduledSessions(ctx, "user-1", "", 0, 20)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "ss-6", rows[0].SessionID)
	assert.Equal(t, "task-1", rows[0].TaskID)

	filtered, err := scheduled.ListScheduledSessions(ctx, "user-2", "task-1", 0, 20)
	require.NoError(t, err)
	assert.Empty(t, filtered)
	_ = store
}

func TestScheduledSubAgentRunTimestampsMigration_UpgradesExistingRows(t *testing.T) {
	store, scheduled, cleanup := setupScheduledStore(t)
	defer cleanup()
	ctx := t.Context()

	// Recreate the run table from the original deployed schema, even after the
	// shared fresh fixture starts applying migration 031.
	_, err := store.db.ExecContext(ctx, "DROP TABLE scheduled_subagent_runs")
	require.NoError(t, err)
	execSQLFile(t, store, "../../../migrations/028_scheduled_session_store.sql")
	session := scheduledTestSession("ss-subagent-upgrade", "occ-subagent-upgrade", "root-run-upgrade")
	_, err = scheduled.CreateScheduledSession(ctx, session)
	require.NoError(t, err)
	instance := testSubAgentInstance(session.SessionID, "w-before-upgrade")
	run := testSubAgentRun(instance, "child-before-upgrade", "", 1)
	run.StartedAt = time.Date(2024, time.January, 2, 3, 4, 5, 123_000_000, time.UTC)
	run.FinishedAt = run.StartedAt.Add(7*time.Second + 456*time.Millisecond)
	require.NoError(t, scheduled.AppendSubAgentRun(ctx, instance, run))

	_, err = scheduled.ListSubAgentRuns(ctx, session.SessionID, instance.AgentInstanceID)
	require.ErrorContains(t, err, "Unknown column 'create_time'")
	execSQLFile(t, store, "../../../migrations/031_scheduled_subagent_run_timestamps.sql")
	got, ok, err := scheduled.GetSubAgentRun(ctx, session.SessionID, instance.AgentInstanceID, run.RunID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, run.StartedAt.Equal(got.CreateTime), "backfill preserves the original start time")
	assert.True(t, run.FinishedAt.Equal(got.UpdateTime), "backfill preserves the original finish time")
	assert.True(t, run.StartedAt.Equal(got.StartedAt))
	assert.True(t, run.FinishedAt.Equal(got.FinishedAt))
	assert.Equal(t, run.Status, got.Status)
	assert.JSONEq(t, string(run.MessagesJSON), string(got.MessagesJSON))

	var columns int
	require.NoError(t, store.db.GetContext(ctx, &columns, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'scheduled_subagent_runs'
		  AND column_name IN ('create_time', 'update_time')
		  AND column_type = 'timestamp(3)' AND is_nullable = 'NO'
		  AND LOWER(column_default) = 'current_timestamp(3)'
	`))
	assert.Equal(t, 2, columns, "both columns match ordinary run precision, nullability and defaults")

	_, err = store.db.ExecContext(ctx, `
		UPDATE scheduled_subagent_runs SET status = ? WHERE session_id = ? AND run_id = ?
	`, model.SubAgentStatusError, session.SessionID, run.RunID)
	require.NoError(t, err)
	updated, ok, err := scheduled.GetSubAgentRun(ctx, session.SessionID, instance.AgentInstanceID, run.RunID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, run.StartedAt.Equal(updated.CreateTime), "ordinary updates preserve create_time")
	assert.True(t, updated.UpdateTime.After(got.UpdateTime), "update_time advances automatically")
	assert.True(t, run.StartedAt.Equal(updated.StartedAt))
	assert.True(t, run.FinishedAt.Equal(updated.FinishedAt))
}
