package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/modelselect"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/schedule"
	"github.com/lush/blowball/pkg/scheduledruntime"
	mysqlstore "github.com/lush/blowball/pkg/store/mysql"
	"github.com/lush/blowball/pkg/turn"
	"github.com/lush/blowball/services/scheduler/internal/scheduler"
	"github.com/lush/blowball/services/scheduler/internal/store"
	"github.com/lush/blowball/services/scheduler/internal/task"
)

// schemaTables is every table the two services share in this test.
var schemaTables = []string{
	"scheduled_attachments_deleted", "scheduled_attachments",
	"scheduled_titles_deleted", "scheduled_titles",
	"scheduled_turn_usage", "scheduled_context_compactions",
	"scheduled_subagent_runs", "scheduled_subagent_instances",
	"scheduled_tool_calls", "scheduled_messages_deleted", "scheduled_messages",
	"scheduled_sessions_deleted", "scheduled_sessions",
	"scheduled_execution_receipts", "scheduled_resource_drafts", "scheduled_resource_versions",
	"scheduled_task_runs", "scheduled_tasks",
}

func setupIntegration(t *testing.T) (*store.Store, *mysqlstore.Store) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping cross-service scheduler integration test")
	}
	st, err := store.New(dsn)
	require.NoError(t, err)
	lockIntegrationSchema(t, st)
	main, err := mysqlstore.New(dsn)
	require.NoError(t, err)
	ctx := context.Background()
	_, _ = st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0")
	for _, tbl := range schemaTables {
		_, _ = st.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}
	_, _ = st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
	for _, file := range []string{
		"../../migrations/026_scheduled_agent_tasks.sql",
		"../../migrations/027_scheduled_service_fields.sql",
		"../../migrations/028_scheduled_session_store.sql",
		"../../migrations/029_scheduled_execution_control.sql",
	} {
		applySQLFile(t, st, file)
	}
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0")
		for _, tbl := range schemaTables {
			_, _ = st.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
		}
		_, _ = st.DB().ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
		_ = st.Close()
		_ = main.Close()
	})
	return st, main
}

func applySQLFile(t *testing.T, st *store.Store, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var stmt strings.Builder
	for line := range strings.SplitSeq(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if _, err := st.DB().ExecContext(context.Background(), stmt.String()); err != nil {
				t.Fatalf("apply %s: %v\n%s", path, err, stmt.String())
			}
			stmt.Reset()
		}
	}
}

const integrationSchemaLock = "blowball_test_store_schema"

func lockIntegrationSchema(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := st.DB().Connx(ctx)
	require.NoError(t, err)
	var acquired sql.NullInt64
	require.NoError(t, conn.GetContext(ctx, &acquired, "SELECT GET_LOCK(?, 60)", integrationSchemaLock))
	require.True(t, acquired.Valid && acquired.Int64 == 1, "could not acquire the MySQL test schema lock")
	t.Cleanup(func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, "SELECT RELEASE_LOCK(?)", integrationSchemaLock)
		_ = conn.Close()
	})
}

type fixedValidator struct{}

func (fixedValidator) ValidateTaskConfig(context.Context, string, string, task.ValidateConfig) (string, string, error) {
	return "model-a", "none", nil
}

type echoUsers struct{}

func (echoUsers) GetUserByID(_ context.Context, userID string) (*model.User, error) {
	return &model.User{UserID: userID, Status: model.UserStatusActive}, nil
}

type fakeTokens struct{}

func (fakeTokens) Issue(context.Context, string) (string, error) { return "exec-token", nil }

type fakeTurn struct {
	mu       sync.Mutex
	requests []*turn.Request
	block    bool
	release  chan struct{}
}

func (f *fakeTurn) Submit(ctx context.Context, req *turn.Request) turn.Submission {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if req.BeforeStart != nil {
		if err := req.BeforeStart(ctx); err != nil {
			return turn.Submission{Outcome: turn.SubmitRejected, Err: err}
		}
	}
	if f.block {
		select {
		case <-f.release:
		case <-ctx.Done():
		}
		// Dead worker: the turn ends without a terminal fact.
		ch := make(chan turn.Result, 1)
		ch <- turn.Result{}
		return turn.Submission{Started: &turn.Started{ResultCh: ch}, Outcome: turn.SubmitStarted}
	}
	if req.OnTerminal != nil {
		req.OnTerminal(ctx, turn.TerminalResult{
			RunID: req.RunID, SessionID: req.SessionID, UserID: req.UserID,
			Status: "done", MessagesDurable: true,
		})
	}
	ch := make(chan turn.Result, 1)
	ch <- turn.Result{}
	return turn.Submission{Started: &turn.Started{ResultCh: ch}, Outcome: turn.SubmitStarted}
}

func (f *fakeTurn) LastRequest() *turn.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

type fakeCanceller struct {
	once    sync.Once
	release chan struct{}
}

func (c *fakeCanceller) RequestCancel(context.Context, string) error {
	c.once.Do(func() { close(c.release) })
	return nil
}

type fixedObserver struct{ alive bool }

func (f fixedObserver) Alive(context.Context, string) (bool, error) { return f.alive, nil }
func (f fixedObserver) InProcess(string) bool                       { return false }

// occurrenceSource adapts the Scheduler's task service/authorization to the
// Runtime's neutral ports.
type occurrenceSource struct {
	tasks *task.Service
	sched *scheduler.Scheduler
}

func (o occurrenceSource) GetOccurrence(ctx context.Context, occurrenceID string) (*scheduledruntime.Occurrence, error) {
	run, err := o.tasks.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		return nil, rpcx.Errorf(rpcx.CodeNotFound, "occurrence not found")
	}
	return &scheduledruntime.Occurrence{
		OccurrenceID: run.OccurrenceID, TaskID: run.TaskID, UserID: run.UserID,
		TaskRevision: run.Revision, SnapshotJSON: run.SnapshotJSON,
		RunID: run.RunID, SessionID: run.SessionID, SessionSource: run.SessionSource,
		Status: run.Status, AttemptCount: run.AttemptCount,
		ResourceRevision: run.ResourceRevision, ManifestHash: run.ManifestHash,
	}, nil
}

func (o occurrenceSource) AuthorizeStart(ctx context.Context, req scheduledruntime.AuthorizeStartRequest) (*scheduledruntime.Grant, error) {
	grant, err := o.sched.AuthorizeStart(ctx, req.OccurrenceID, req.RunID, req.SessionID,
		req.Attempt, req.ReceiptGeneration, req.RuntimeWorkerGeneration)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &scheduledruntime.Grant{
		OccurrenceID: grant.OccurrenceID, Generation: grant.Generation,
		RunID: grant.RunID, SessionID: grant.SessionID,
		StartedAt: grant.StartedAt, Deadline: grant.Deadline,
	}, nil
}

func (o occurrenceSource) GetStartGrant(ctx context.Context, occurrenceID string, generation uint64) (*scheduledruntime.Grant, error) {
	grant, err := o.sched.GetStartGrant(ctx, occurrenceID, generation)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &scheduledruntime.Grant{
		OccurrenceID: grant.OccurrenceID, Generation: grant.Generation,
		RunID: grant.RunID, SessionID: grant.SessionID,
		StartedAt: grant.StartedAt, Deadline: grant.Deadline,
	}, nil
}

func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrOccurrenceNotFound), errors.Is(err, store.ErrGrantNotFound):
		return rpcx.Errorf(rpcx.CodeNotFound, "%s", err.Error())
	case errors.Is(err, store.ErrTaskUnavailable), errors.Is(err, store.ErrGrantFenced):
		return rpcx.Errorf(rpcx.CodeStartRejected, "%s", err.Error())
	default:
		return rpcx.Errorf(rpcx.CodeUnavailable, "%s", err.Error())
	}
}

type runtimeClient struct{ rt *scheduledruntime.Runtime }

func (c runtimeClient) SubmitScheduledRun(ctx context.Context, in scheduler.SubmitInput) (*scheduler.SubmitResult, error) {
	res, err := c.rt.Submit(ctx, scheduledruntime.SubmitInput{
		OccurrenceID: in.OccurrenceID, TaskID: in.TaskID, RunID: in.RunID,
		SessionID: in.SessionID, Attempt: in.Attempt, ConfigFingerprint: in.ConfigFingerprint,
	})
	if err != nil {
		return nil, err
	}
	return &scheduler.SubmitResult{
		ReceiptGeneration: res.Receipt.ReceiptGeneration,
		Status:            res.Receipt.Status,
		AlreadyAccepted:   res.AlreadyAccepted,
	}, nil
}

func (c runtimeClient) GetScheduledExecution(ctx context.Context, occurrenceID string) (*scheduler.ExecutionFact, error) {
	rec, err := c.rt.GetExecution(ctx, occurrenceID)
	if err != nil {
		if de, ok := rpcx.AsError(err); ok && de.Code == rpcx.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &scheduler.ExecutionFact{
		Status: rec.Status, Reason: rec.Reason, LastError: rec.LastError,
		ReceiptGeneration: rec.ReceiptGeneration, Terminal: rec.Terminal(),
		Started: rec.StartIntentAt != nil, Uncertain: rec.Status == model.ScheduledReceiptUncertain,
		StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		GrantGeneration: rec.GrantGeneration, GrantDeadline: rec.GrantDeadline,
		StopProofPresent: rec.StopProofPresent, StopProofGeneration: rec.StopProofGen,
	}, nil
}

type harness struct {
	schedStore *store.Store
	mainStore  *mysqlstore.Store
	tasks      *task.Service
	sched      *scheduler.Scheduler
	runtime    *scheduledruntime.Runtime
	turns      *fakeTurn
}

func TestSchedulerRuntimeHappyPath(t *testing.T) {
	turns := &fakeTurn{}
	h := buildHarness(t, turns, fixedObserver{alive: true}, &fakeCanceller{release: make(chan struct{})})
	ctx := context.Background()

	taskRow := h.createDueTask(t)
	h.start()
	defer h.stop()

	require.True(t, h.waitForStatus(ctx, taskRow.TaskID, model.ScheduledRunSucceeded, 10*time.Second),
		"occurrence did not succeed")
	req := turns.LastRequest()
	require.NotNil(t, req)
	assert.Equal(t, model.SessionSourceScheduled, req.Source)
	assert.Equal(t, "do it", req.Content)

	occurrence := h.firstOccurrence(t, taskRow.TaskID)
	receipt, err := h.mainStore.GetScheduledExecution(ctx, occurrence.OccurrenceID)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	assert.True(t, receipt.Terminal())
	assert.True(t, receipt.StopProofPresent)
}

func TestSchedulerRuntimeUncertainHoldAndStopProof(t *testing.T) {
	turns := &fakeTurn{block: true, release: make(chan struct{})}
	canceller := &fakeCanceller{release: turns.release}
	h := buildHarness(t, turns, fixedObserver{alive: false}, canceller)
	ctx := context.Background()

	taskRow := h.createDueTask(t)
	h.start()
	defer h.stop()

	require.True(t, h.waitForStatus(ctx, taskRow.TaskID, model.ScheduledRunInterrupted, 10*time.Second),
		"unknown execution was not projected as interrupted")
	occurrence := h.firstOccurrence(t, taskRow.TaskID)
	assert.Equal(t, model.ScheduledReasonResultUnknown, occurrence.Reason)

	taskAfter, err := h.schedStore.GetTaskInternal(ctx, taskRow.TaskID)
	require.NoError(t, err)
	assert.Equal(t, occurrence.OccurrenceID, taskAfter.ExecutionHoldOccurrenceID, "the task must be held")

	receipt, err := h.mainStore.GetScheduledExecution(ctx, occurrence.OccurrenceID)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	assert.Equal(t, model.ScheduledReceiptUncertain, receipt.Status)

	// Controlled reconciliation proves the stop; the Scheduler CAS-clears the
	// matching hold on its next pass.
	ok, err := h.runtime.ProveStopped(ctx, occurrence.OccurrenceID, receipt.ReceiptGeneration, model.ScheduledStopProofReconciled)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, h.waitForHoldCleared(ctx, taskRow.TaskID, 10*time.Second), "hold was not cleared by the stop proof")
}

// buildHarness constructs the Runtime with the real occurrence source and the
// Scheduler with the real runtime client.
func buildHarness(t *testing.T, turns *fakeTurn, observer fixedObserver, canceller *fakeCanceller) *harness {
	t.Helper()
	schedStore, mainStore := setupIntegration(t)
	tasks := task.New(schedStore, fixedValidator{}, 5000)
	selection := modelselect.Config{
		Catalog: []config.ModelCatalogEntry{{Name: "model-a", MaxContextTokens: 100000, MaxCompletionTokens: 8000}},
		Default: "model-a", DefaultEffort: "none",
	}
	var sched *scheduler.Scheduler
	occ := occurrenceSource{tasks: tasks, sched: nil}
	rt, err := scheduledruntime.New(scheduledruntime.Dependencies{
		Store: mainStore, Occurrences: occurrenceSourceRef{&occ}, Users: echoUsers{}, Turns: turns,
		Canceller: canceller, Observer: observer, Tokens: fakeTokens{}, Selection: selection,
		Config: scheduledruntime.Config{
			PollInterval: 20 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
			MaxConcurrent: 2, MaxPrepareAttempts: 3, ScanBatch: 10, DefaultRunDuration: time.Second,
		},
	})
	require.NoError(t, err)
	sched = scheduler.New(schedStore, runtimeClient{rt: rt}, scheduler.Config{
		PollInterval: 20 * time.Millisecond, LeaseDuration: 5 * time.Second,
		MaxRunDuration: 2 * time.Second, MaxConcurrent: 2, ScanBatch: 10,
		MaxAttempts: 3, RetryBackoff: 10 * time.Millisecond, ExecutionEnabled: true,
	})
	occ.sched = sched
	return &harness{schedStore: schedStore, mainStore: mainStore, tasks: tasks, sched: sched, runtime: rt, turns: turns}
}

// occurrenceSourceRef lets the occurrence source see the Scheduler constructed
// after the Runtime (the two services are mutually dependent by design).
type occurrenceSourceRef struct{ src *occurrenceSource }

func (r occurrenceSourceRef) GetOccurrence(ctx context.Context, id string) (*scheduledruntime.Occurrence, error) {
	return r.src.GetOccurrence(ctx, id)
}
func (r occurrenceSourceRef) AuthorizeStart(ctx context.Context, req scheduledruntime.AuthorizeStartRequest) (*scheduledruntime.Grant, error) {
	return r.src.AuthorizeStart(ctx, req)
}
func (r occurrenceSourceRef) GetStartGrant(ctx context.Context, id string, gen uint64) (*scheduledruntime.Grant, error) {
	return r.src.GetStartGrant(ctx, id, gen)
}

func scheduleCron() schedule.Rule {
	return schedule.Rule{Type: schedule.TypeCron, Expression: "* * * * *", Timezone: "UTC"}
}

func (h *harness) createDueTask(t *testing.T) *model.ScheduledTask {
	t.Helper()
	ctx := context.Background()
	created, err := h.tasks.Create(ctx, task.CreateInput{
		ActorUserID: "user-1", Name: "nightly", Prompt: "do it",
		Schedule: scheduleCron(),
	})
	require.NoError(t, err)
	// Make the task due now.
	loaded, err := h.schedStore.GetTaskInternal(ctx, created.TaskID)
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Minute)
	loaded.NextRunAt = &now
	_, err = h.schedStore.UpdateTask(ctx, loaded, loaded.Revision)
	require.NoError(t, err)
	return created
}

func (h *harness) start() {
	h.sched.Start()
	h.runtime.Start()
}

func (h *harness) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.runtime.Shutdown(ctx)
	_ = h.sched.Shutdown(ctx)
}

func (h *harness) firstOccurrence(t *testing.T, taskID string) model.ScheduledTaskRun {
	t.Helper()
	rows, err := h.schedStore.ListOccurrences(context.Background(), "user-1", taskID, 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	return rows[0]
}

func (h *harness) waitForStatus(ctx context.Context, taskID, status string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := h.schedStore.ListOccurrences(ctx, "user-1", taskID, 10, 0)
		if err == nil {
			for _, r := range rows {
				if r.Status == status {
					return true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (h *harness) waitForHoldCleared(ctx context.Context, taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		taskRow, err := h.schedStore.GetTaskInternal(ctx, taskID)
		if err == nil && taskRow != nil && taskRow.ExecutionHoldOccurrenceID == "" {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
