package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/artifact"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/scheduler"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/store/fs"
	mysqlstore "github.com/lush/blowball/internal/store/mysql"
	redisstore "github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/turn"
)

func setupScheduledIntegrationMySQL(t *testing.T) *mysqlstore.Store {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping scheduled integration test")
	}
	cfg, err := drivermysql.ParseDSN(dsn)
	require.NoError(t, err)
	cfg.ParseTime = true
	cfg.MultiStatements = true
	cfg.Loc = time.UTC
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	require.NoError(t, err)
	defer admin.Close()
	dbName := fmt.Sprintf("blowball_sched_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE DATABASE `" + dbName + "`")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE `" + dbName + "`") })
	cfg.DBName = dbName
	store, err := mysqlstore.New(cfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	entries, err := os.ReadDir("../../migrations")
	require.NoError(t, err)
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		if name == "006_messages_reasoning.sql" {
			continue // legacy no-op on a fresh schema; MySQL 8.0 lacks DROP COLUMN IF EXISTS.
		}
		raw, err := os.ReadFile(filepath.Join("../../migrations", name))
		require.NoError(t, err)
		_, err = store.DB().ExecContext(context.Background(), string(raw))
		require.NoError(t, err, name)
	}
	return store
}

type scheduledInnerAgent struct {
	userID string
	token  string
}

func (a *scheduledInnerAgent) Handle(ctx context.Context, workspaceRoot, _ string, _ string, _ []agent.Message, hub *stream.Hub, _ agent.RoundHook, _ agent.ModelOverride) error {
	a.userID = agent.UserIDFromContext(ctx)
	a.token = skillmarket.TokenFromContext(ctx)
	hub.Send(stream.AgentStartEvent(stream.AgentConfucius))
	hub.Send(stream.TokenEvent(stream.AgentConfucius, "Working"))
	hub.Send(stream.ToolCallEvent(stream.AgentConfucius, "call-report", "xizhi_write_file", map[string]any{"path": "report.txt"}))
	hub.Send(stream.ToolResultEvent(stream.AgentConfucius, "call-report", `{"status":0,"result":"written"}`))
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "report.txt"), []byte("scheduled report"), 0o644); err != nil {
		return err
	}
	hub.Send(stream.TokenEvent(stream.AgentConfucius, "Done [report](blowball://workspace/report.txt)"))
	hub.Send(stream.AgentEndEvent(stream.AgentConfucius))
	hub.Send(stream.DoneEvent(map[string]any{
		"total":    map[string]any{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
		"by_agent": map[string]any{"Confucius": map[string]any{"total_tokens": 30}},
		"meta":     map[string]any{"context_tokens": 42, "parallel": false, "sub_agent_invocations": []string{}},
	}))
	return nil
}

func TestScheduledTaskExecutesOfflineAndPersistsHistory(t *testing.T) {
	mysql := setupScheduledIntegrationMySQL(t)
	ctx := context.Background()
	_, err := mysql.DB().ExecContext(ctx, `INSERT INTO users (user_id, username, password, status) VALUES ('user-1', 'user-1', 'hash', 'active')`)
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	redis, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redis.Close() })
	dataDir := t.TempDir()
	filesystem, err := fs.New(dataDir)
	require.NoError(t, err)
	deps := service.SessionDeps{MySQL: mysql, Redis: redis, FS: filesystem}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	runMgr := run.NewManager(redis.RunStore(), run.NewRegistry())
	inner := &scheduledInnerAgent{}
	executor := turn.NewExecutor(turn.Dependencies{
		Sessions: sessSvc, Messages: msgSvc, Orchestrator: turn.NewOrchestratorAdapterFromRunner(inner),
		DataDir: dataDir, Runs: runMgr, Artifacts: artifact.NewService(nil, mysql, 1<<20),
	})
	selection := modelselect.Config{
		Catalog: []config.ModelCatalogEntry{{Name: "gpt-test", Thinking: true, MaxContextTokens: 100000, MaxCompletionTokens: 10000}},
		Default: "gpt-test", DefaultEffort: "none",
	}
	management := service.NewScheduledTaskService(mysql, selection, 5000)
	task, err := management.Create(ctx, "user-1", service.CreateScheduledTaskInput{
		Name: "offline report", Prompt: "write the scheduled report",
		Schedule: schedule.Rule{Type: schedule.TypeAt, RunAt: time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano), Timezone: "UTC"},
	})
	require.NoError(t, err)

	runtime := scheduler.New(scheduler.Dependencies{
		Store: mysql, Turns: executor, Sessions: sessSvc,
		Tokens: service.NewExecutionTokenIssuer("integration-secret", 5*time.Minute),
		Runs:   runMgr, ModelSelection: selection,
		Config: scheduler.Config{PollInterval: 10 * time.Millisecond, LeaseDuration: time.Minute, MaxRunDuration: time.Minute, MaxConcurrent: 1, MaxAttempts: 3, RetryBackoff: time.Millisecond},
	})
	runtime.Start()
	t.Cleanup(func() {
		executor.StopNewTurns()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})

	var occurrence model.ScheduledTaskRun
	require.Eventually(t, func() bool {
		runs, err := management.Runs(ctx, "user-1", task.TaskID, 20, 0)
		if err != nil || len(runs) != 1 {
			return false
		}
		occurrence = runs[0]
		return occurrence.Status == model.ScheduledRunSucceeded
	}, 10*time.Second, 20*time.Millisecond, "offline task did not reach a durable successful terminal state")

	require.Equal(t, "user-1", inner.userID)
	require.NotEmpty(t, inner.token, "the execution JWT reaches only the in-memory market context")
	messages, err := mysql.ListMessages(ctx, occurrence.SessionID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(messages), 4)
	var sawUser, sawTool, sawArtifact bool
	for _, message := range messages {
		if message.Role == model.RoleUser && strings.Contains(message.Content, "write the scheduled report") {
			sawUser = true
		}
		if message.EventType == model.EventTypeToolCall && message.ToolCallID == "call-report" {
			sawTool = true
		}
		if message.EventType == model.EventTypeArtifact {
			sawArtifact = true
			require.Contains(t, message.Content, "report.txt")
		}
		require.NotContains(t, message.Content, inner.token)
	}
	require.True(t, sawUser, "user message is durable without an SSE client")
	require.True(t, sawTool, "tool activity is durable")
	require.True(t, sawArtifact, "workspace artifact declaration is durable")
	calls, err := mysql.ListToolCalls(ctx, occurrence.SessionID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "call-report", calls[0].ToolCallID)
	require.NotContains(t, calls[0].Arguments, inner.token, "tool arguments must not contain the execution JWT")
	require.NotContains(t, calls[0].Result, inner.token)
	var usage model.TurnUsage
	require.NoError(t, mysql.DB().GetContext(ctx, &usage, `SELECT trace_id, session_id, user_id, model, total_tokens, context_tokens FROM turn_usage WHERE trace_id = ?`, occurrence.RunID))
	require.Equal(t, occurrence.RunID, usage.TraceID)
	require.Equal(t, "gpt-test", usage.Model)
	require.Equal(t, 30, usage.TotalTokens)
	require.Equal(t, 42, usage.ContextTokens)

	// The terminal task row was written after message persistence. Its MySQL
	// history is independent of the short Redis replay retain window.
	mr.FastForward(70 * time.Second)
	_, found, err := redis.RunStore().GetMeta(ctx, occurrence.RunID)
	require.NoError(t, err)
	require.False(t, found, "run replay metadata expired")
	history, err := management.Runs(ctx, "user-1", task.TaskID, 20, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, model.ScheduledRunSucceeded, history[0].Status)
}

type timeoutInnerAgent struct{}

func (a *timeoutInnerAgent) Handle(ctx context.Context, _, _ string, _ string, _ []agent.Message, hub *stream.Hub, _ agent.RoundHook, _ agent.ModelOverride) error {
	hub.Send(stream.AgentStartEvent(stream.AgentConfucius))
	hub.Send(stream.TokenEvent(stream.AgentConfucius, "partial scheduled output"))
	<-ctx.Done()
	return ctx.Err()
}

func TestScheduledTaskTimeoutPersistsPartialOutput(t *testing.T) {
	mysql := setupScheduledIntegrationMySQL(t)
	ctx := context.Background()
	_, err := mysql.DB().ExecContext(ctx, `INSERT INTO users (user_id, username, password, status) VALUES ('user-1', 'user-1', 'hash', 'active')`)
	require.NoError(t, err)
	mr := miniredis.RunT(t)
	redis, err := redisstore.New(mr.Addr(), "", 0, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = redis.Close() })
	dataDir := t.TempDir()
	filesystem, err := fs.New(dataDir)
	require.NoError(t, err)
	deps := service.SessionDeps{MySQL: mysql, Redis: redis, FS: filesystem}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)
	runMgr := run.NewManager(redis.RunStore(), run.NewRegistry())
	executor := turn.NewExecutor(turn.Dependencies{
		Sessions: sessSvc, Messages: msgSvc,
		Orchestrator: turn.NewOrchestratorAdapterFromRunner(&timeoutInnerAgent{}), DataDir: dataDir, Runs: runMgr,
	})
	selection := modelselect.Config{
		Catalog: []config.ModelCatalogEntry{{Name: "gpt-test", Thinking: true, MaxContextTokens: 100000, MaxCompletionTokens: 10000}},
		Default: "gpt-test", DefaultEffort: "none",
	}
	management := service.NewScheduledTaskService(mysql, selection, 5000)
	task, err := management.Create(ctx, "user-1", service.CreateScheduledTaskInput{
		Name: "timeout report", Prompt: "start then wait",
		Schedule: schedule.Rule{Type: schedule.TypeAt, RunAt: time.Now().Add(50 * time.Millisecond).UTC().Format(time.RFC3339Nano), Timezone: "UTC"},
	})
	require.NoError(t, err)
	runtime := scheduler.New(scheduler.Dependencies{
		Store: mysql, Turns: executor, Sessions: sessSvc,
		Tokens: service.NewExecutionTokenIssuer("integration-secret", time.Minute), Runs: runMgr, ModelSelection: selection,
		Config: scheduler.Config{PollInterval: 5 * time.Millisecond, LeaseDuration: time.Minute, MaxRunDuration: 30 * time.Millisecond, MaxConcurrent: 1, MaxAttempts: 1, RetryBackoff: time.Millisecond},
	})
	runtime.Start()
	t.Cleanup(func() {
		executor.StopNewTurns()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})

	var occurrence model.ScheduledTaskRun
	require.Eventually(t, func() bool {
		runs, err := management.Runs(ctx, "user-1", task.TaskID, 20, 0)
		if err != nil || len(runs) != 1 {
			return false
		}
		occurrence = runs[0]
		return occurrence.Status == model.ScheduledRunCancelled
	}, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, model.ScheduledReasonTaskTimeout, occurrence.Reason)
	messages, err := mysql.ListMessages(ctx, occurrence.SessionID)
	require.NoError(t, err)
	var final *model.Message
	for i := range messages {
		if messages[i].Role == model.RoleAssistant && messages[i].EventType == model.EventTypeMessage {
			final = &messages[i]
		}
	}
	require.NotNil(t, final, "partial assistant final row must be durable")
	require.Equal(t, "partial scheduled output", final.Content)
	require.Equal(t, model.MessageStatusInterrupted, final.Status)
}
