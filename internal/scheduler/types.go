package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/turn"
)

// Store is the durable scheduling surface. Redis is used only by the shared
// turn admission/run lifecycle, never as the source of future task truth.
type Store interface {
	MaterializeScheduledTasks(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error)
	ListClaimableScheduledRuns(ctx context.Context, now time.Time, limit int) ([]model.ScheduledTaskRun, error)
	ListScheduledRunsForRecovery(ctx context.Context, limit int) ([]model.ScheduledTaskRun, error)
	ClaimScheduledRun(ctx context.Context, occurrenceID string, now time.Time, lease time.Duration, maxAttempts int) (*model.ScheduledTaskRun, bool, error)
	GetScheduledTaskRun(ctx context.Context, occurrenceID string) (*model.ScheduledTaskRun, error)
	GetScheduledTaskRunByRunID(ctx context.Context, runID string) (*model.ScheduledTaskRun, error)
	ScheduledRunAuthorization(ctx context.Context, occurrenceID string) (string, error)
	MarkScheduledRunLaunching(ctx context.Context, occurrenceID, claimToken string, now time.Time) (bool, error)
	CrossScheduledStartFence(ctx context.Context, occurrenceID, claimToken string, now time.Time) (bool, error)
	ScheduleScheduledRunRetry(ctx context.Context, occurrenceID, claimToken string, nextAttempt time.Time, lastError string) (bool, error)
	FinishScheduledRunBeforeStart(ctx context.Context, occurrenceID, claimToken, status, reason, lastError string, now time.Time) (bool, error)
	FinishScheduledRun(ctx context.Context, occurrenceID, status, reason, lastError string, now time.Time) (bool, error)
	PauseScheduledTask(ctx context.Context, taskID, reason string, now time.Time) error
}

// SessionPreparer idempotently creates or revalidates the reserved session.
type SessionPreparer interface {
	EnsureSessionWithID(ctx context.Context, userID, sessionID, traceID string) (*model.Session, error)
}

// TokenIssuer signs a bounded execution identity after authorization.
type TokenIssuer interface {
	Issue(ctx context.Context, userID string) (string, error)
}

// TurnSubmitter is the shared headless turn entry point.
type TurnSubmitter interface {
	Submit(ctx context.Context, req *turn.Request) turn.Submission
}

// Config bounds all scheduler loops and executions.
type Config struct {
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	MaxRunDuration time.Duration
	MaxConcurrent  int
	ScanBatch      int
	MaxAttempts    int
	RetryBackoff   time.Duration
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.MaxRunDuration <= 0 {
		c.MaxRunDuration = 10 * time.Minute
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.ScanBatch <= 0 || c.ScanBatch > 100 {
		c.ScanBatch = 100
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = time.Second
	}
	return c
}

// Dependencies stay separate from HTTP/Gin and from the task-management API.
type Dependencies struct {
	Store          Store
	Turns          TurnSubmitter
	Sessions       SessionPreparer
	Tokens         TokenIssuer
	Runs           *run.Manager
	ModelSelection modelselect.Config
	Config         Config
	Now            func() time.Time
}

// Scheduler owns polling, in-flight capacity, and graceful cancellation.
type Scheduler struct {
	store     Store
	turns     TurnSubmitter
	sessions  SessionPreparer
	tokens    TokenIssuer
	runs      *run.Manager
	selection modelselect.Config
	cfg       Config
	now       func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
	activeMu  sync.Mutex
	active    map[string]context.CancelFunc
}

func New(deps Dependencies) *Scheduler {
	cfg := deps.Config.withDefaults()
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		store: deps.Store, turns: deps.Turns, sessions: deps.Sessions,
		tokens: deps.Tokens, runs: deps.Runs, selection: deps.ModelSelection,
		cfg: cfg, now: now, ctx: ctx, cancel: cancel,
		sem: make(chan struct{}, cfg.MaxConcurrent), active: map[string]context.CancelFunc{},
	}
}
