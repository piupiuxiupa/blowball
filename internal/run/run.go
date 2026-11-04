// Package run owns the turn-run lifecycle introduced by the
// turn-detach-resume capability: a run is one accepted message request's
// agent turn, identified by its run id (the request's trace_id). The run's
// execution context is held server-side (Registry) so it survives the HTTP
// connection that started it, and its observable state lives in Redis (Store)
// so cancellation, resume, and session-busy checks work across processes.
//
// Redis key families (all TTL-bearing; see the lifecycle constants below):
//
//	run:{rid}:events  — Stream, one entry per turn event (incl. done); Redis
//	                    writes use RunKeyTTLWithJitter
//	run:{rid}:meta    — Hash: session_id / user_id / status / model / created_at;
//	                    Redis writes use RunKeyTTLWithJitter
//	run:{rid}:alive   — heartbeat, short TTL refreshed by the drainer
//	run:{rid}:cancel  — cross-process cancel flag, consumed by the drainer;
//	                    Redis writes use RunKeyTTLWithJitter
//	session:{sid}:run — single-active-run claim (SET NX) with SessionClaimTTL,
//	                    compare-and-rearmed by the drainer heartbeat
package run

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/lush/blowball/internal/model"
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
	// StatusQueued marks a run whose message sits in the session's queue
	// awaiting its turn (session-message-queue). It is neither running (no
	// heartbeat exists by design, so dead-run detection must ignore it) nor
	// terminal; the pump flips it to running when it starts the turn.
	StatusQueued = "queued"
)

// Shared lifecycle timings. The heartbeat TTL is 3× the beat interval so a
// single missed tick does not declare a live run dead; the run-key TTL is the
// run-key family's crash-backstop baseline (no terminal bookkeeping ran);
// RetainAfterTerminal keeps a finished run's event log replayable for late
// attach before expiry.
const (
	// HeartbeatEvery is the drainer's heartbeat / cancel-poll period.
	HeartbeatEvery = 5 * time.Second
	// HeartbeatTTL is the alive-key TTL (3× HeartbeatEvery).
	HeartbeatTTL = 15 * time.Second
	// RunKeyTTL is the run-key family crash-backstop baseline. Redis writes
	// add jitter via RunKeyTTLWithJitter; memstore uses this value directly.
	RunKeyTTL = 1 * time.Hour
	// SessionClaimTTL is the session claim's independent crash backstop. Do
	// not adjust it with RunKeyTTL: it bounds how long a crashed process can
	// leave a session locked, while heartbeats keep a live long turn claimed.
	SessionClaimTTL = 30 * time.Minute
	// RetainAfterTerminal is how long a terminal run's event log and meta
	// survive before cleanup, so a late attach can replay the finished turn.
	RetainAfterTerminal = 60 * time.Second
	// MaxStreamLen bounds run:{rid}:events entries (approximate MAXLEN).
	MaxStreamLen = 100000
)

// RunKeyTTLWithJitter returns RunKeyTTL with a uniform ±10% offset. Redis
// write sites call this independently so keys created together do not expire
// in lockstep. MemStore intentionally uses the deterministic baseline instead.
func RunKeyTTLWithJitter() time.Duration {
	offset := int64(RunKeyTTL) / 10
	return RunKeyTTL + time.Duration(rand.Int64N(2*offset+1)-offset)
}

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

// QueueEntry is one queued message snapshot (session-message-queue): the
// resolved-request equivalent stored in the Redis LIST `queue:{sid}` until a
// pump starts it as the next turn. The snapshot (not the raw request
// parameters) is what travels, so the pumping process — which may be a
// different replica — never re-resolves the request against its own config
// reading order. RunID is the enqueueing request's trace_id, so the SSE
// response of the queued POST and any later resume attach to the same run.
//
// RawToken carries the enqueueing request's login JWT so the pumped turn can
// keep the skill-market allowlist fetch (and every other token-gated tool
// execution) working with no HTTP request in hand. It rests in Redis within
// the same trust boundary as the user's message content itself, it is
// TTL-bound by the JWT's own expiry, and a turn outliving it degrades through
// the standing fail-closed path — identical to a long turn whose request token
// expired mid-flight.
type QueueEntry struct {
	RunID       string                       `json:"run_id"`
	SessionID   string                       `json:"session_id"`
	UserID      string                       `json:"user_id"`
	Content     string                       `json:"content"`
	Model       string                       `json:"model"`
	Selection   QueueSelection               `json:"selection"`
	Attachments []model.MessageAttachmentRef `json:"attachments,omitempty"`
	RawToken    string                       `json:"raw_token,omitempty"`
	EnqueuedAt  time.Time                    `json:"enqueued_at"`
}

// QueueSelection is the queue-entry form of a turn's resolved model selection
// (the handler's modelSelection): model/effort axes, wire-family and vision
// markers, and the per-entry context/output budgets including the
// finish_reason=length continuation policy. Field names are chosen (not the
// config struct's) so the JSON stays a stable wire shape.
type QueueSelection struct {
	Model               string `json:"model"`
	Thinking            bool   `json:"thinking"`
	Vision              bool   `json:"vision"`
	Effort              string `json:"effort"`
	MaxContextTokens    int    `json:"max_context_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	LengthExpandStep    int    `json:"length_expand_step,omitempty"`
	LengthMaxRetries    int    `json:"length_max_retries,omitempty"`
}

// AdmitOutcome enumerates AdmitMessage's results.
type AdmitOutcome int

const (
	// AdmitClaimed: the claim was free and the queue empty, so the caller's
	// run now holds the session claim and must start its turn immediately
	// (the fast path — byte-identical to a successful ClaimSession).
	AdmitClaimed AdmitOutcome = iota
	// AdmitQueued: a turn is running or the queue is non-empty, so the entry
	// was appended at the queue tail.
	AdmitQueued
	// AdmitFull: the queue already holds maxDepth entries.
	AdmitFull
)

// AdmitResult reports AdmitMessage's outcome. Position is the 1-based queue
// position of the newly enqueued entry (0 otherwise).
type AdmitResult struct {
	Outcome  AdmitOutcome
	Position int
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
	// ClaimBackgroundSession atomically claims an idle session only when its
	// FIFO is also empty. Unlike interactive admission, any store error is
	// fail-closed: the caller must not start a model without mutual exclusion.
	ClaimBackgroundSession(ctx context.Context, sessionID, runID string) (holder string, ok bool, err error)
	// ReleaseSession deletes the claim when it still points at runID, so a
	// stale releaser cannot unlock a session already reclaimed by a newer run.
	ReleaseSession(ctx context.Context, sessionID, runID string) error
	// ActiveRuns maps each sessionID to its current active run id ("" when
	// none). Used by the session list generating flag.
	ActiveRuns(ctx context.Context, sessionIDs []string) (map[string]string, error)

	// ── session message queue (session-message-queue) ──────────────────────
	//
	// Queue admission/continuation primitives. The FIFO invariant — "queue
	// non-empty ⇒ a new message may only RPUSH; claim free AND queue empty ⇒
	// a message may claim directly" — is sealed atomically inside each
	// implementation (Redis: one Lua script per operation; MemStore: one lock
	// critical section). Failures are surfaced as errors for the caller to
	// WARN — like the rest of the Store they never block a message.

	// AdmitMessage atomically admits one queued-message candidate to a
	// session: claims the session run slot when it is free AND the queue is
	// empty (the caller must then start its turn immediately, exactly like a
	// successful ClaimSession), otherwise — in the SAME atomic step as the
	// RPUSH — initializes the run's meta (status queued, ownership and model
	// fields from the entry) and appends the `queued` event to
	// run:{rid}:events, so the entry is never consumable before its
	// observable state exists (a pump that wins the race right after the
	// admission can never see the initial state written late — review fix:
	// an unconditional follow-up InitMeta could overwrite running/done).
	// Reports AdmitQueued (Position = the entry's 1-based depth) or
	// AdmitFull when the queue already holds maxDepth entries.
	AdmitMessage(ctx context.Context, sessionID string, entry QueueEntry, maxDepth int) (AdmitResult, error)

	// PumpHead atomically pops the queue head and transfers the session claim
	// to it when the claim is free. ok=false means nothing was pumped: empty
	// queue, or the claim is held (a racing admission won it first — the head
	// stays enqueued). Called after a turn's terminal bookkeeping released its
	// claim, and by the reconciler for crash residue.
	PumpHead(ctx context.Context, sessionID string) (entry QueueEntry, ok bool, err error)

	// RequeueHead atomically returns a popped entry to the queue FRONT and
	// releases the session claim when it still points at the entry's run
	// (shutdown aborted the startup between the pop and the spawn — the
	// post-restart reconciler re-pumps the head). When the claim already
	// moved on, the entry is still requeued; it waits for the next free slot.
	RequeueHead(ctx context.Context, sessionID string, entry QueueEntry) error

	// DequeueMessage removes the queued entry identified by runID (cancel of a
	// queued run). ok=false when no such entry exists. The claim is untouched
	// (a queued run never held it).
	DequeueMessage(ctx context.Context, sessionID, runID string) (entry QueueEntry, ok bool, err error)

	// QueueLen reports the session's queue depth (0 when no key exists).
	QueueLen(ctx context.Context, sessionID string) (int, error)
	// QueueEntries returns the session's queued entries in enqueue order.
	QueueEntries(ctx context.Context, sessionID string) ([]QueueEntry, error)

	// QueuedSessions lists every session that currently has a queued entry
	// (the reconciler's scan surface; unordered).
	QueuedSessions(ctx context.Context) ([]string, error)

	// TouchQueued re-arms the run-key family TTL (events/meta) for a queued
	// run without touching the heartbeat or the claim — the queue's wait can
	// outlast RunKeyTTL, and the reconciler keeps the replay surface alive.
	TouchQueued(ctx context.Context, runID string) error

	// ClearQueue drops every queued entry of the session (session deletion)
	// AND terminates each dropped entry's run — status cancelled plus the
	// terminal retain window — so subscribers tailing those runs close
	// promptly instead of waiting out the meta TTL (the entries can never be
	// pumped after the clear). Entries are not message rows and are never
	// archived.
	ClearQueue(ctx context.Context, sessionID string) error

	// Heartbeat refreshes run:{rid}:alive (HeartbeatTTL), re-arms the
	// event/meta TTLs (RunKeyTTLWithJitter in Redis), and compare-and-rearms
	// the session claim (SessionClaimTTL) when it still points at runID, so a
	// long turn never expires or loses its mutual exclusion mid-flight.
	Heartbeat(ctx context.Context, runID, sessionID string) error
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
