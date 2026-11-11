package scheduledruntime

import (
	"context"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/turn"
)

// ReceiptStore is the authoritative Runtime execution-fact store (MySQL). It
// is implemented by *mysql.Store.
type ReceiptStore interface {
	AcceptScheduledExecution(ctx context.Context, receipt model.ScheduledExecutionReceipt, session model.Session) (*model.ScheduledExecutionReceipt, bool, error)
	GetScheduledExecution(ctx context.Context, occurrenceID string) (*model.ScheduledExecutionReceipt, error)
	GetScheduledExecutionBySession(ctx context.Context, sessionID string) (*model.ScheduledExecutionReceipt, error)
	ListClaimableScheduledReceipts(ctx context.Context, now time.Time, limit int) ([]model.ScheduledExecutionReceipt, error)
	ListLiveScheduledReceipts(ctx context.Context, limit int) ([]model.ScheduledExecutionReceipt, error)
	ClaimScheduledReceipt(ctx context.Context, occurrenceID, workerGeneration string, lease time.Duration, maxActive int, now time.Time) (*model.ScheduledExecutionReceipt, bool, error)
	RenewScheduledReceiptLease(ctx context.Context, occurrenceID, workerGeneration string, lease time.Duration, now time.Time) (bool, error)
	ReleaseScheduledReceiptForRetry(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, reason, lastError string, maxAttempts int, now time.Time) (bool, error)
	ReleaseScheduledReceiptOnShutdown(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string) (bool, error)
	MarkScheduledReceiptStartIntent(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string, lease time.Duration, now time.Time) (bool, error)
	MarkScheduledReceiptRunning(ctx context.Context, occurrenceID string, generation uint64, workerGeneration string, grant model.ScheduledStartGrant) (bool, error)
	FinishScheduledReceipt(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error)
	FinishScheduledReceiptBeforeStart(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error)
	FinishScheduledReceiptStartRejected(ctx context.Context, occurrenceID string, generation uint64, workerGeneration, status, reason, lastError string, now time.Time) (bool, error)
	MarkScheduledReceiptUncertain(ctx context.Context, occurrenceID string, generation uint64, reason, lastError string, now time.Time) (bool, error)
	RecordScheduledStopProof(ctx context.Context, occurrenceID string, generation uint64, kind string, now time.Time) (bool, error)
	CancelScheduledReceiptBeforeStart(ctx context.Context, occurrenceID string, generation uint64, reason, lastError string, now time.Time) (bool, error)
	CountActiveScheduledReceipts(ctx context.Context) (int, error)
}

// OccurrenceSource is the Scheduler-owned authoritative occurrence/start
// surface. It is implemented by SchedulerClient over the versioned gRPC
// client.
type OccurrenceSource interface {
	GetOccurrence(ctx context.Context, occurrenceID string) (*Occurrence, error)
	AuthorizeStart(ctx context.Context, req AuthorizeStartRequest) (*Grant, error)
	GetStartGrant(ctx context.Context, occurrenceID string, generation uint64) (*Grant, error)
}

// Occurrence is the neutral projection of the authoritative scheduler
// occurrence. The Runtime re-validates every field it executes against.
type Occurrence struct {
	OccurrenceID     string
	TaskID           string
	UserID           string
	TaskRevision     int64
	SnapshotJSON     string
	RunID            string
	SessionID        string
	SessionSource    string // "scheduled" | "legacy_interactive" | ""
	Status           string
	AttemptCount     int
	ResourceRevision uint64
	ManifestHash     string
	ScheduledFor     time.Time
}

// AuthorizeStartRequest carries the stable execution identity. It contains no
// user credential.
type AuthorizeStartRequest struct {
	OccurrenceID            string
	RunID                   string
	SessionID               string
	Attempt                 int
	ReceiptGeneration       uint64
	ClaimFenceID            string
	RuntimeWorkerGeneration string
}

// Grant is the unique start authorization.
type Grant struct {
	OccurrenceID string
	Generation   uint64
	RunID        string
	SessionID    string
	StartedAt    time.Time
	Deadline     time.Time
}

// UserLookup validates that the owner is still active at acceptance and at
// start time.
type UserLookup interface {
	GetUserByID(ctx context.Context, userID string) (*model.User, error)
}

// TurnSubmitter is the shared headless turn entry point (*turn.Executor).
type TurnSubmitter interface {
	Submit(ctx context.Context, req *turn.Request) turn.Submission
}

// Canceller requests cancellation of one run. An in-process registry hit
// cancels the turn context directly; otherwise it raises the Redis cancel
// flag. Neither is a stop proof.
type Canceller interface {
	RequestCancel(ctx context.Context, runID string) error
}

// RunObserver reports run liveness for reconciliation.
type RunObserver interface {
	Alive(ctx context.Context, runID string) (bool, error)
	InProcess(runID string) bool
}

// ResourceValidator verifies a pinned published resource version and returns
// the bounded resource context injected into the task prompt. Phase 5 wires
// the production implementation; nil means resources are unavailable.
type ResourceValidator interface {
	ValidatePinned(ctx context.Context, userID, taskID string, revision uint64, manifestHash string) (contextText string, err error)
}

// TokenIssuer mints the bounded in-memory execution identity after
// authorization (the existing service.ExecutionTokenIssuer).
type TokenIssuer interface {
	Issue(ctx context.Context, userID string) (string, error)
}
