package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/turn"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeStore struct {
	mu               sync.Mutex
	nextID           int
	tasks            map[string]*model.ScheduledTask
	runs             map[string]*model.ScheduledTaskRun
	authReason       map[string]string
	crossErr         error
	crossCommitError bool
	finishErrs       int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]*model.ScheduledTask{}, runs: map[string]*model.ScheduledTaskRun{}, authReason: map[string]string{}}
}

func cloneRun(run *model.ScheduledTaskRun) *model.ScheduledTaskRun {
	if run == nil {
		return nil
	}
	cp := *run
	return &cp
}

func (s *fakeStore) addTask(task *model.ScheduledTask) {
	s.tasks[task.TaskID] = task
}

func (s *fakeStore) MaterializeScheduledTasks(_ context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tasks []*model.ScheduledTask
	for _, task := range s.tasks {
		if task.Enabled && task.DeletedAt == nil && task.NextRunAt != nil && !task.NextRunAt.After(now) {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].NextRunAt.Before(*tasks[j].NextRunAt) })
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	var out []model.ScheduledTaskRun
	for _, task := range tasks {
		active := false
		for _, run := range s.runs {
			if run.TaskID == task.TaskID && !model.ScheduledRunTerminal(run.Status) {
				active = true
				break
			}
		}
		s.nextID++
		scheduledFor := *task.NextRunAt
		sessionID := fmt.Sprintf("session-%d", s.nextID)
		if task.SessionMode == model.ScheduledSessionExisting && task.SessionID != nil {
			sessionID = *task.SessionID
		}
		snapshot := task.Snapshot()
		if snapshot.Session.Mode == model.ScheduledSessionNew {
			snapshot.Session.SessionID = sessionID
		}
		raw, _ := snapshotJSON(snapshot)
		nextAttempt := now
		run := &model.ScheduledTaskRun{
			ID: int64(s.nextID), OccurrenceID: fmt.Sprintf("occurrence-%d", s.nextID),
			TaskID: task.TaskID, UserID: task.UserID, Revision: task.Revision,
			SnapshotJSON: raw, ScheduledFor: scheduledFor, RunID: fmt.Sprintf("run-%d", s.nextID),
			SessionID: sessionID, SessionMode: task.SessionMode, Status: model.ScheduledRunPending,
			NextAttemptAt: &nextAttempt, CreateTime: now, UpdateTime: now,
		}
		if active {
			run.Status = model.ScheduledRunSkipped
			run.Reason = model.ScheduledReasonTaskOverlap
			run.NextAttemptAt = nil
			run.FinishedAt = &now
		}
		s.runs[run.OccurrenceID] = run
		out = append(out, *cloneRun(run))
		if task.ScheduleType == schedule.TypeCron {
			next, _ := schedule.Next(task.Rule(), now)
			task.NextRunAt = &next
		} else {
			task.NextRunAt = nil
		}
	}
	return out, nil
}

func (s *fakeStore) ListClaimableScheduledRuns(_ context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.ScheduledTaskRun
	for _, run := range s.runs {
		claimable := (run.Status == model.ScheduledRunPending && (run.NextAttemptAt == nil || !run.NextAttemptAt.After(now))) ||
			(run.Status == model.ScheduledRunPreparing && run.LeaseUntil != nil && !run.LeaseUntil.After(now))
		if claimable {
			out = append(out, *cloneRun(run))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledFor.Before(out[j].ScheduledFor) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) ListScheduledRunsForRecovery(_ context.Context, limit int) ([]model.ScheduledTaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.ScheduledTaskRun
	for _, run := range s.runs {
		if run.Status == model.ScheduledRunLaunching || run.Status == model.ScheduledRunRunning {
			out = append(out, *cloneRun(run))
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) ClaimScheduledRun(_ context.Context, occurrenceID string, now time.Time, lease time.Duration, maxAttempts int) (*model.ScheduledTaskRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if run == nil || run.AttemptCount >= maxAttempts {
		return nil, false, nil
	}
	claimable := (run.Status == model.ScheduledRunPending && (run.NextAttemptAt == nil || !run.NextAttemptAt.After(now))) ||
		(run.Status == model.ScheduledRunPreparing && run.LeaseUntil != nil && !run.LeaseUntil.After(now))
	if !claimable {
		return nil, false, nil
	}
	s.nextID++
	run.Status = model.ScheduledRunPreparing
	run.ClaimToken = fmt.Sprintf("claim-%d", s.nextID)
	leaseUntil := now.Add(lease)
	run.LeaseUntil = &leaseUntil
	run.NextAttemptAt = nil
	run.AttemptCount++
	run.UpdateTime = now
	return cloneRun(run), true, nil
}

func (s *fakeStore) GetScheduledTaskRun(_ context.Context, occurrenceID string) (*model.ScheduledTaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRun(s.runs[occurrenceID]), nil
}
func (s *fakeStore) GetScheduledTaskRunByRunID(_ context.Context, runID string) (*model.ScheduledTaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		if run.RunID == runID {
			return cloneRun(run), nil
		}
	}
	return nil, nil
}
func (s *fakeStore) ScheduledRunAuthorization(_ context.Context, occurrenceID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if run == nil {
		return model.ScheduledReasonStartRejected, nil
	}
	if reason := s.authReason[run.TaskID]; reason != "" {
		return reason, nil
	}
	task := s.tasks[run.TaskID]
	if task == nil || task.DeletedAt != nil {
		return model.ScheduledReasonTaskDeleted, nil
	}
	if !task.Enabled {
		return model.ScheduledReasonTaskPaused, nil
	}
	return "", nil
}
func (s *fakeStore) MarkScheduledRunLaunching(_ context.Context, occurrenceID, claimToken string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if run == nil || run.Status != model.ScheduledRunPreparing || run.ClaimToken != claimToken || run.LeaseUntil == nil || !run.LeaseUntil.After(now) {
		return false, nil
	}
	if s.authReason[run.TaskID] != "" || !s.tasks[run.TaskID].Enabled {
		return false, nil
	}
	run.Status = model.ScheduledRunLaunching
	run.UpdateTime = now
	return true, nil
}
func (s *fakeStore) CrossScheduledStartFence(_ context.Context, occurrenceID, claimToken string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if s.crossErr != nil && !s.crossCommitError {
		return false, s.crossErr
	}
	if run == nil || run.Status != model.ScheduledRunLaunching || run.ClaimToken != claimToken || run.LeaseUntil == nil || !run.LeaseUntil.After(now) || s.authReason[run.TaskID] != "" || !s.tasks[run.TaskID].Enabled {
		return false, nil
	}
	run.Status = model.ScheduledRunRunning
	started := now
	run.StartedAt = &started
	run.LeaseUntil = nil
	run.UpdateTime = now
	if s.crossCommitError {
		return false, s.crossErr
	}
	return true, nil
}
func (s *fakeStore) ScheduleScheduledRunRetry(_ context.Context, occurrenceID, claimToken string, nextAttempt time.Time, lastError string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if run == nil || run.ClaimToken != claimToken || run.StartedAt != nil || (run.Status != model.ScheduledRunPreparing && run.Status != model.ScheduledRunLaunching) {
		return false, nil
	}
	run.Status = model.ScheduledRunPending
	run.ClaimToken = ""
	run.LeaseUntil = nil
	run.NextAttemptAt = &nextAttempt
	run.LastError = lastError
	run.UpdateTime = nextAttempt
	return true, nil
}
func (s *fakeStore) FinishScheduledRunBeforeStart(_ context.Context, occurrenceID, claimToken, status, reason, lastError string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[occurrenceID]
	if run == nil || run.ClaimToken != claimToken || run.StartedAt != nil || (run.Status != model.ScheduledRunPreparing && run.Status != model.ScheduledRunLaunching) {
		return false, nil
	}
	finishRunLocked(run, status, reason, lastError, now)
	return true, nil
}
func (s *fakeStore) FinishScheduledRun(_ context.Context, occurrenceID, status, reason, lastError string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishErrs > 0 {
		s.finishErrs--
		return false, errors.New("terminal write unavailable")
	}
	run := s.runs[occurrenceID]
	if run == nil || (run.Status != model.ScheduledRunLaunching && run.Status != model.ScheduledRunRunning) {
		return false, nil
	}
	finishRunLocked(run, status, reason, lastError, now)
	return true, nil
}
func finishRunLocked(run *model.ScheduledTaskRun, status, reason, lastError string, now time.Time) {
	run.Status = status
	run.Reason = reason
	run.LastError = lastError
	run.FinishedAt = &now
	run.ClaimToken = ""
	run.LeaseUntil = nil
	run.NextAttemptAt = nil
	run.UpdateTime = now
}
func (s *fakeStore) PauseScheduledTask(_ context.Context, taskID, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[taskID]
	task.Enabled = false
	task.PausedReason = reason
	for _, run := range s.runs {
		if run.TaskID == taskID && run.StartedAt == nil && (run.Status == model.ScheduledRunPending || run.Status == model.ScheduledRunPreparing || run.Status == model.ScheduledRunLaunching) {
			finishRunLocked(run, model.ScheduledRunCancelled, reason, "", now)
		}
	}
	return nil
}

func snapshotJSON(snapshot model.ScheduledTaskSnapshot) (string, error) {
	raw, err := json.Marshal(snapshot)
	return string(raw), err
}

type fakeSessions struct {
	mu        sync.Mutex
	byID      map[string]*model.Session
	calls     []string
	ensureErr error
}

func newFakeSessions() *fakeSessions { return &fakeSessions{byID: map[string]*model.Session{}} }
func (f *fakeSessions) EnsureSessionWithID(_ context.Context, userID, sessionID, traceID string) (*model.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	f.calls = append(f.calls, sessionID)
	if existing := f.byID[sessionID]; existing != nil {
		if existing.UserID != userID {
			return nil, errors.New("owner mismatch")
		}
		return existing, nil
	}
	sess := &model.Session{SessionID: sessionID, UserID: userID, TraceID: traceID}
	f.byID[sessionID] = sess
	return sess, nil
}

type fakeTokens struct {
	mu    sync.Mutex
	users []string
	err   error
}

func (f *fakeTokens) Issue(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.users = append(f.users, userID)
	return "sk-runtime-secret", nil
}

type fakeTurns struct {
	mu        sync.Mutex
	runs      *run.Manager
	outcome   turn.SubmitOutcome
	err       error
	block     bool
	release   chan struct{}
	requests  []*turn.Request
	userIDs   []string
	status    string
	resultErr error
}

func newFakeTurns(runs *run.Manager) *fakeTurns {
	return &fakeTurns{runs: runs, outcome: turn.SubmitStarted, release: make(chan struct{}), status: run.StatusDone}
}

func (f *fakeTurns) Submit(ctx context.Context, req *turn.Request) turn.Submission {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.userIDs = append(f.userIDs, agentUserID(ctx))
	outcome, submitErr := f.outcome, f.err
	f.mu.Unlock()
	if outcome != turn.SubmitStarted {
		return turn.Submission{Outcome: outcome, Err: submitErr}
	}
	if req.BeforeStart != nil {
		if err := req.BeforeStart(ctx); err != nil {
			outcome := turn.SubmitRejected
			if errors.Is(err, turn.ErrShutdownDuringStartup) {
				outcome = turn.SubmitShutdown
			}
			return turn.Submission{Outcome: outcome, Err: err}
		}
	}
	resultCh := make(chan turn.Result, 1)
	turnCtx, cancel := context.WithCancel(context.Background())
	regRun := f.runs.Registry.Register(req.RunID, cancel)
	started := &turn.Started{ResultCh: resultCh, TurnDone: regRun.Done()}
	go func() {
		defer f.runs.Registry.Unregister(req.RunID)
		defer regRun.Finish()
		status := f.status
		var err error
		if f.block {
			select {
			case <-f.release:
			case <-turnCtx.Done():
				status = run.StatusCancelled
				err = turnCtx.Err()
			}
		}
		if f.resultErr != nil {
			err = f.resultErr
			status = run.StatusError
		}
		if req.OnTerminal != nil {
			req.OnTerminal(context.Background(), turn.TerminalResult{RunID: req.RunID, SessionID: req.SessionID, UserID: req.UserID, Status: status, Err: err})
		}
		resultCh <- turn.Result{Err: err}
	}()
	return turn.Submission{Started: started, Outcome: turn.SubmitStarted}
}

func agentUserID(ctx context.Context) string {
	return agent.UserIDFromContext(ctx)
}

func schedulerFixture(t *testing.T) (*Scheduler, *fakeStore, *fakeTurns, *fakeSessions, *fakeTokens, *fakeClock, *run.Manager) {
	t.Helper()
	store := newFakeStore()
	clock := newFakeClock()
	registry := run.NewRegistry()
	manager := run.NewManager(run.NewMemStore(), registry)
	turns := newFakeTurns(manager)
	sessions := newFakeSessions()
	tokens := &fakeTokens{}
	selection := modelselect.Config{}
	selection.Default = "thinking"
	selection.DefaultEffort = "none"
	selection.Catalog = testCatalog()
	s := New(Dependencies{
		Store: store, Turns: turns, Sessions: sessions, Tokens: tokens, Runs: manager,
		ModelSelection: selection, Now: clock.Now,
		Config: Config{PollInterval: 5 * time.Millisecond, LeaseDuration: time.Minute, MaxRunDuration: time.Minute, MaxConcurrent: 4, MaxAttempts: 3, RetryBackoff: time.Second},
	})
	return s, store, turns, sessions, tokens, clock, manager
}

func testCatalog() []config.ModelCatalogEntry {
	return []config.ModelCatalogEntry{{Name: "thinking", Thinking: true, MaxContextTokens: 100000, MaxCompletionTokens: 10000}}
}

func cronTask(id string) *model.ScheduledTask {
	task := &model.ScheduledTask{TaskID: id, UserID: "user-1", Name: id, Prompt: "report", SessionMode: model.ScheduledSessionNew, Enabled: true, Revision: 1}
	task.SetSchedule(schedule.Rule{Type: schedule.TypeCron, Expression: "30 9 * * *", Timezone: "UTC"})
	next := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	task.NextRunAt = &next
	task.ApplyDefaults()
	return task
}

func waitSchedulerIdle(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.waitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func storedRun(t *testing.T, store *fakeStore, occurrenceID string) *model.ScheduledTaskRun {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	run := cloneRun(store.runs[occurrenceID])
	if run == nil {
		t.Fatalf("missing run %s", occurrenceID)
	}
	return run
}
