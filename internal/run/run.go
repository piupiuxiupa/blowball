// Package run owns the turn-run lifecycle introduced by the
// turn-detach-resume capability: a run is one accepted message request's
// agent turn, identified by its run id (the request's trace_id). The run's
// execution context is held server-side (Registry) so it survives the HTTP
// connection that started it, and its observable state lives in Redis (Store)
// so cancellation, resume, and session-busy checks work across processes.
//
// Redis key families (all TTL-bearing; see docs in design.md of the change):
//
//	run:{rid}:events  — Stream, one entry per turn event (incl. done)
//	run:{rid}:meta    — Hash: session_id / user_id / status / model / created_at
//	run:{rid}:alive   — heartbeat, short TTL refreshed by the drainer
//	run:{rid}:cancel  — cross-process cancel flag, consumed by the drainer
//	session:{sid}:run — single-active-run claim (SET NX) + generating marker
package run

import (
	"context"
	"time"

	"github.com/lush/blowball/internal/stream"
)

// Run lifecycle statuses stored in run:{rid}:meta's status field.
const (
	// StatusRunning marks a run whose owning process has not reported a
	// terminal state. A status-running run whose heartbeat expired is dead
	// (crashed process) and may be force-cleared by any observer.
	StatusRunning = "running"
	// StatusDone marks a run that completed normally (done event emitted).
	StatusDone = "done"
	// StatusError marks a run that ended with an orchestrator failure.
	StatusError = "error"
	// StatusCancelled marks a run ended by the cancel endpoint (in-process,
	// cross-replica, or dead-run force clear of an already-cancelled run).
	StatusCancelled = "cancelled"
	// StatusInterrupted marks a run whose process died mid-turn; assigned by
	// whichever observer first detects the expired heartbeat.
	StatusInterrupted = "interrupted"
)

// Shared lifecycle timings. The heartbeat TTL is 3× the beat interval so a
// single missed tick does not declare a live run dead; the run-key TTL is the
// crash backstop (no terminal bookkeeping ran); RetainAfterTerminal keeps a
// finished run's event log replayable for late attach before expiry.
const (
	// HeartbeatEvery is the drainer's heartbeat / cancel-poll period.
	HeartbeatEvery = 5 * time.Second
	// HeartbeatTTL is the alive-key TTL (3× HeartbeatEvery).
	HeartbeatTTL = 15 * time.Second
	// RunKeyTTL bounds every run key family when no terminal cleanup ran
	// (process crash) so a session can never stay locked longer than this.
	RunKeyTTL = 30 * time.Minute
	// RetainAfterTerminal is how long a terminal run's event log and meta
	// survive before cleanup, so a late attach can replay the finished turn.
	RetainAfterTerminal = 60 * time.Second
	// MaxStreamLen bounds run:{rid}:events entries (approximate MAXLEN).
	MaxStreamLen = 100000
)

// Terminal reports whether status is a terminal run state.
func Terminal(status string) bool {
	switch status {
	case StatusDone, StatusError, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Entry is one event log record with its Redis Stream entry id. The entry id
// doubles as the SSE `id:` line so Last-Event-ID resume is exact.
type Entry struct {
	ID    string
	Event stream.StreamEvent
}

// RunMeta is the persisted run descriptor (run:{rid}:meta).
type RunMeta struct {
	SessionID string
	UserID    string
	Status    string
	Model     string
	CreatedAt string
}

// Store is the Redis-backed run state surface. Implementations must be safe
// for concurrent use. Every method is best-effort state management: failures
// are surfaced as errors for the caller to WARN — they never block a turn.
type Store interface {
	// AppendEvent adds one event to run:{rid}:events (created with RunKeyTTL
	// on first append). The stream entry id is assigned by the store.
	AppendEvent(ctx context.Context, runID string, e stream.StreamEvent) error

	// ReadEvents returns the entries strictly after `after` ("" or "0" reads
	// from the beginning). When no entries are available it waits up to block
	// (0 = return immediately) for new ones. after must be a valid stream id
	// or empty; callers sanitize externally-sourced ids first.
	ReadEvents(ctx context.Context, runID, after string, block time.Duration) ([]Entry, error)

	// InitMeta creates run:{rid}:meta with the given fields and RunKeyTTL.
	InitMeta(ctx context.Context, runID string, m RunMeta) error
	// SetStatus updates the meta status field (refreshing the TTL).
	SetStatus(ctx context.Context, runID, status string) error
	// GetMeta loads the meta hash; ok=false when the key does not exist.
	GetMeta(ctx context.Context, runID string) (RunMeta, bool, error)

	// ClaimSession atomically claims the single active run slot for a session
	// (SET NX). ok=false means the claim failed and holder is the current
	// holder's run id (possibly empty when unreadable).
	ClaimSession(ctx context.Context, sessionID, runID string) (holder string, ok bool, err error)
	// ReleaseSession deletes the claim when it still points at runID, so a
	// stale releaser cannot unlock a session already reclaimed by a newer run.
	ReleaseSession(ctx context.Context, sessionID, runID string) error
	// ActiveRuns maps each sessionID to its current active run id ("" when
	// none). Used by the session list generating flag.
	ActiveRuns(ctx context.Context, sessionIDs []string) (map[string]string, error)

	// Heartbeat refreshes run:{rid}:alive (HeartbeatTTL) and re-arms the
	// event/meta TTLs (RunKeyTTL) so a long turn never expires mid-flight.
	Heartbeat(ctx context.Context, runID string) error
	// Alive reports whether the heartbeat key exists (TTL not expired).
	Alive(ctx context.Context, runID string) (bool, error)

	// SetCancelFlag raises the cross-process cancel flag for the run.
	SetCancelFlag(ctx context.Context, runID string) error
	// TakeCancelFlag atomically reads-and-clears the cancel flag.
	TakeCancelFlag(ctx context.Context, runID string) (bool, error)

	// Retire moves a terminal run's keys to a retain-window TTL and deletes
	// the heartbeat / cancel keys immediately (the run is over; only the
	// replayable event log and meta stay for late attach).
	Retire(ctx context.Context, runID string, retain time.Duration) error
}

// Manager bundles the process-local Registry with the shared Store. Handlers
// hold one Manager; the api role constructs only the Store (for the list
// generating flag) and never a Registry.
type Manager struct {
	Store    Store
	Registry *Registry
}

// NewManager builds a Manager. A nil registry is tolerated (api-role use:
// list-marker reads only); Cancel paths must check for it.
func NewManager(store Store, reg *Registry) *Manager {
	return &Manager{Store: store, Registry: reg}
}

// Finalize performs the terminal bookkeeping sequence for a run whose owning
// process observed the terminal outcome: status first (unblocks waiting SSE
// readers), then the session release (unblocks SESSION_BUSY), then the
// retain-window expiry. Every step is WARN-on-failure — the RunKeyTTL is the
// backstop that eventually frees the session.
func (m *Manager) Finalize(ctx context.Context, runID, sessionID, status string) {
	if err := m.Store.SetStatus(ctx, runID, status); err != nil {
		warn("run finalize: set status failed", runID, err)
	}
	if err := m.Store.ReleaseSession(ctx, sessionID, runID); err != nil {
		warn("run finalize: release session failed", runID, err)
	}
	if err := m.Store.Retire(ctx, runID, RetainAfterTerminal); err != nil {
		warn("run finalize: retire failed", runID, err)
	}
}
