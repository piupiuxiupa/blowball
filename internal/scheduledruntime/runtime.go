package scheduledruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/pkg/logger"
)

// Config bounds the Runtime's scheduling loops and admitted concurrency.
type Config struct {
	PollInterval       time.Duration
	LeaseDuration      time.Duration
	MaxConcurrent      int
	ScanBatch          int
	MaxPrepareAttempts int
	// DefaultRunDuration is the fallback watchdog budget when a grant carries
	// no deadline (the Scheduler always sets one in production).
	DefaultRunDuration time.Duration
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 30 * time.Second
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.ScanBatch <= 0 {
		c.ScanBatch = 50
	}
	if c.MaxPrepareAttempts <= 0 {
		c.MaxPrepareAttempts = 3
	}
	if c.DefaultRunDuration <= 0 {
		c.DefaultRunDuration = 10 * time.Minute
	}
	return c
}

// Dependencies are the Runtime's collaborators.
type Dependencies struct {
	Store       ReceiptStore
	Occurrences OccurrenceSource
	Users       UserLookup
	Turns       TurnSubmitter
	Canceller   Canceller
	// Observer is optional; nil disables heartbeat-based reconciliation (the
	// runtime then only relies on the owning worker's terminal write).
	Observer RunObserver
	// Resources is optional; a task carrying a resource revision is rejected
	// when this is nil.
	Resources ResourceValidator
	Tokens    TokenIssuer
	Selection modelselect.Config
	Config    Config
	Now       func() time.Time
	// WorkerID identifies this process in worker generations. Defaults to a
	// random hex id.
	WorkerID string
}

// Runtime owns the main-service side of scheduled execution: durable
// idempotent acceptance, fail-closed admission, start authorization, the
// grant-timed watchdog, terminal facts and conservative reconciliation.
type Runtime struct {
	store       ReceiptStore
	occurrences OccurrenceSource
	users       UserLookup
	turns       TurnSubmitter
	canceller   Canceller
	observer    RunObserver
	resources   ResourceValidator
	tokens      TokenIssuer
	selection   modelselect.Config
	cfg         Config
	now         func() time.Time
	workerID    string

	startOnce sync.Once
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	sem       chan struct{}
}

// New validates required dependencies and builds the Runtime.
func New(deps Dependencies) (*Runtime, error) {
	if deps.Store == nil || deps.Occurrences == nil || deps.Users == nil ||
		deps.Turns == nil || deps.Canceller == nil || deps.Tokens == nil {
		return nil, fmt.Errorf("scheduledruntime: store, occurrences, users, turns, canceller and tokens are required")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	workerID := deps.WorkerID
	if workerID == "" {
		workerID = randomID()
	}
	cfg := deps.Config.withDefaults()
	return &Runtime{
		store: deps.Store, occurrences: deps.Occurrences, users: deps.Users,
		turns: deps.Turns, canceller: deps.Canceller, observer: deps.Observer,
		resources: deps.Resources, tokens: deps.Tokens, selection: deps.Selection,
		cfg: cfg, now: now, workerID: workerID,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}, nil
}

// Start launches the polling/reconciliation loop.
func (r *Runtime) Start() {
	r.startOnce.Do(func() {
		r.ctx, r.cancel = context.WithCancel(context.Background())
		r.wg.Add(1)
		go r.loop()
	})
}

// Shutdown stops claiming new work and waits for in-flight worker functions
// (their turns are owned and bounded by the shared turn executor's own
// shutdown gate).
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	r.cycle(r.ctx)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.cycle(r.ctx)
		}
	}
}

// cycle claims every currently claimable receipt into the local capacity and
// runs one reconciliation pass.
func (r *Runtime) cycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	receipts, err := r.store.ListClaimableScheduledReceipts(ctx, r.now(), r.cfg.ScanBatch)
	if err != nil {
		logWarn(ctx, "scheduled receipt scan failed", err)
		return
	}
	for _, rec := range receipts {
		if ctx.Err() != nil {
			return
		}
		if !r.acquire() {
			break
		}
		rec := rec
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.release()
			r.execute(ctx, rec)
		}()
	}
	r.reconcile(ctx)
}

func (r *Runtime) acquire() bool {
	select {
	case r.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *Runtime) release() { <-r.sem }

// reconcile converts live-but-unreachable executions to `uncertain`. It never
// replays them: only a generation-bound terminal write or a controlled stop
// proof can finish an uncertain receipt.
func (r *Runtime) reconcile(ctx context.Context) {
	if r.observer == nil || ctx.Err() != nil {
		return
	}
	live, err := r.store.ListLiveScheduledReceipts(ctx, r.cfg.ScanBatch)
	if err != nil {
		logWarn(ctx, "scheduled live-receipt scan failed", err)
		return
	}
	now := r.now()
	for _, rec := range live {
		if r.observer.InProcess(rec.RunID) {
			continue
		}
		alive, err := r.observer.Alive(ctx, rec.RunID)
		if err != nil {
			logWarn(ctx, "scheduled run liveness read failed", err)
			continue
		}
		if alive {
			continue
		}
		since := rec.AcceptedAt
		if rec.StartIntentAt != nil {
			since = *rec.StartIntentAt
		}
		if rec.StartedAt != nil {
			since = *rec.StartedAt
		}
		if now.Sub(since) < r.cfg.LeaseDuration {
			continue // startup grace: a heartbeat may not exist yet
		}
		ok, err := r.store.MarkScheduledReceiptUncertain(ctx, rec.OccurrenceID, rec.ReceiptGeneration,
			model.ScheduledReasonResultUnknown, "run liveness heartbeat absent", now)
		if err != nil {
			logWarn(ctx, "mark scheduled execution uncertain failed", err)
			continue
		}
		if ok {
			logWarn(ctx, "scheduled execution marked uncertain; it will not be replayed",
				fmt.Errorf("occurrence %s run %s", rec.OccurrenceID, rec.RunID))
		}
	}
}

// ProveStopped is the controlled reconciliation entry point: it records a
// generation-bound positive stop proof for an uncertain execution. It is not
// reachable through the public HTTP/RPC surface.
func (r *Runtime) ProveStopped(ctx context.Context, occurrenceID string, generation uint64, kind string) (bool, error) {
	if kind == "" {
		kind = model.ScheduledStopProofReconciled
	}
	return r.store.RecordScheduledStopProof(ctx, occurrenceID, generation, kind, r.now())
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func logWarn(ctx context.Context, msg string, err error) {
	// Kept in one place so the runtime's log calls never include a snapshot or
	// credential; only the stable message and the error text.
	logger.FromContext(ctx).Warn(msg, zap.String("op", "scheduledruntime"), zap.Error(err))
}
