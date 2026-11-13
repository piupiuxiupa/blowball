package run

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/pkg/stream"
)

// MemStore is an in-memory Store implementation for tests and api-role
// bootstrap without Redis. It reproduces the Redis semantics that matter to
// the run lifecycle: SET NX claim with compare-and-release, TTL expiry on
// meta/events/alive, exclusive-cursor reads, and read-and-clear cancel flags.
//
// Reads poll at a small granularity instead of blocking natively (tests only
// need correctness, not wake latency; production uses the Redis Store whose
// XREAD BLOCK wakes instantly). Now is overridable so TTL behaviour is
// testable without real time.
type MemStore struct {
	Now func() time.Time

	mu       sync.Mutex
	streams  map[string][]memEntry
	meta     map[string]RunMeta
	expire   map[string]time.Time // key expiry for streams/meta entries (runID-keyed)
	alive    map[string]time.Time
	cancels  map[string]bool
	claims   map[string]string // sessionID -> runID
	claimExp map[string]time.Time
	queues   map[string][]QueueEntry // sessionID -> queued entries (FIFO)
}

type memEntry struct {
	Entry
	expires time.Time
}

// NewMemStore builds an empty MemStore using the wall clock.
func NewMemStore() *MemStore {
	return &MemStore{
		Now:      time.Now,
		streams:  make(map[string][]memEntry),
		meta:     make(map[string]RunMeta),
		queues:   make(map[string][]QueueEntry),
		expire:   make(map[string]time.Time),
		alive:    make(map[string]time.Time),
		cancels:  make(map[string]bool),
		claims:   make(map[string]string),
		claimExp: make(map[string]time.Time),
	}
}

func (m *MemStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// expired reports whether the run's key family expired (and lazily clears it).
func (m *MemStore) expiredLocked(runID string) bool {
	exp, ok := m.expire[runID]
	if !ok {
		return true
	}
	if m.now().After(exp) {
		delete(m.streams, runID)
		delete(m.meta, runID)
		delete(m.expire, runID)
		delete(m.alive, runID)
		return true
	}
	return false
}

// AppendEvent implements Store.
func (m *MemStore) AppendEvent(_ context.Context, runID string, e stream.StreamEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expiredLocked(runID)
	seq := len(m.streams[runID]) + 1
	id := strconv.Itoa(seq) + "-1"
	m.streams[runID] = append(m.streams[runID], memEntry{
		Entry:   Entry{ID: id, Event: e},
		expires: m.now().Add(RunKeyTTL),
	})
	m.expire[runID] = m.now().Add(RunKeyTTL)
	return nil
}

// ReadEvents implements Store. The after cursor compares by the numeric
// sequence prefix of the entry id ("<seq>-<n>").
func (m *MemStore) ReadEvents(_ context.Context, runID, after string, block time.Duration) ([]Entry, error) {
	deadline := m.now().Add(block)
	for {
		m.mu.Lock()
		m.expiredLocked(runID)
		entries := m.streams[runID]
		afterSeq := 0
		if s := strings.SplitN(after, "-", 2); s[0] != "" && s[0] != "0" {
			if n, err := strconv.Atoi(s[0]); err == nil {
				afterSeq = n
			}
		}
		out := make([]Entry, 0, len(entries))
		for _, ent := range entries {
			seq, _ := strconv.Atoi(strings.SplitN(ent.ID, "-", 2)[0])
			if seq > afterSeq {
				out = append(out, ent.Entry)
			}
		}
		m.mu.Unlock()
		if len(out) > 0 || block <= 0 || !m.now().Before(deadline) {
			return out, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// InitMeta implements Store.
func (m *MemStore) InitMeta(_ context.Context, runID string, meta RunMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.expiredLocked(runID) {
		if _, exists := m.meta[runID]; exists {
			return nil // first writer wins, mirroring a fresh HSET on new key
		}
	}
	m.meta[runID] = meta
	m.expire[runID] = m.now().Add(RunKeyTTL)
	return nil
}

// SetStatus implements Store.
func (m *MemStore) SetStatus(_ context.Context, runID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(runID) {
		return nil // expired keys are a no-op write
	}
	meta, ok := m.meta[runID]
	if !ok {
		return nil
	}
	meta.Status = status
	m.meta[runID] = meta
	return nil
}

// GetMeta implements Store.
func (m *MemStore) GetMeta(_ context.Context, runID string) (RunMeta, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expiredLocked(runID) {
		return RunMeta{}, false, nil
	}
	meta, ok := m.meta[runID]
	return meta, ok, nil
}

// ClaimSession implements Store (SET NX with SessionClaimTTL).
func (m *MemStore) ClaimSession(_ context.Context, sessionID, runID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.claims[sessionID]; ok && m.now().Before(m.claimExp[sessionID]) {
		return holder, false, nil
	}
	m.claims[sessionID] = runID
	m.claimExp[sessionID] = m.now().Add(SessionClaimTTL)
	return runID, true, nil
}

// ClaimBackgroundSession implements Store (idle claim + empty FIFO in one lock).
func (m *MemStore) ClaimBackgroundSession(_ context.Context, sessionID, runID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, ok := m.claims[sessionID]; ok && m.now().Before(m.claimExp[sessionID]) {
		return holder, false, nil
	}
	if len(m.queues[sessionID]) > 0 {
		holder := ""
		if len(m.queues[sessionID]) > 0 {
			holder = m.queues[sessionID][0].RunID
		}
		return holder, false, nil
	}
	m.claims[sessionID] = runID
	m.claimExp[sessionID] = m.now().Add(SessionClaimTTL)
	return runID, true, nil
}

// ReleaseSession implements Store (compare-and-delete).
func (m *MemStore) ReleaseSession(_ context.Context, sessionID, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claims[sessionID] == runID {
		delete(m.claims, sessionID)
		delete(m.claimExp, sessionID)
	}
	return nil
}

// ActiveRuns implements Store (MGET over session claims).
func (m *MemStore) ActiveRuns(_ context.Context, sessionIDs []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(sessionIDs))
	for _, sid := range sessionIDs {
		if holder, ok := m.claims[sid]; ok && m.now().Before(m.claimExp[sid]) {
			out[sid] = holder
		}
	}
	return out, nil
}

// Heartbeat implements Store.
func (m *MemStore) Heartbeat(_ context.Context, runID, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive[runID] = m.now().Add(HeartbeatTTL)
	if _, ok := m.expire[runID]; ok && !m.expiredLocked(runID) {
		m.expire[runID] = m.now().Add(RunKeyTTL)
	}
	if m.claims[sessionID] == runID && m.now().Before(m.claimExp[sessionID]) {
		m.claimExp[sessionID] = m.now().Add(SessionClaimTTL)
	}
	return nil
}

// claimHeldLocked reports whether the session claim currently excludes
// admission (present and unexpired).
func (m *MemStore) claimHeldLocked(sessionID string) bool {
	_, ok := m.claims[sessionID]
	return ok && m.now().Before(m.claimExp[sessionID])
}

// AdmitMessage implements Store (one lock critical section = the atomic
// claim-or-enqueue admission, including the queued publication: meta + the
// queued event land with the RPUSH, never after it — a pump that pops the
// entry the instant it is queued can never observe the initial state
// arriving late).
func (m *MemStore) AdmitMessage(_ context.Context, sessionID string, entry QueueEntry, maxDepth int) (AdmitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.claimHeldLocked(sessionID) && len(m.queues[sessionID]) == 0 {
		m.claims[sessionID] = entry.RunID
		m.claimExp[sessionID] = m.now().Add(SessionClaimTTL)
		return AdmitResult{Outcome: AdmitClaimed}, nil
	}
	if len(m.queues[sessionID]) >= maxDepth {
		return AdmitResult{Outcome: AdmitFull}, nil
	}
	m.queues[sessionID] = append(m.queues[sessionID], entry)
	pos := len(m.queues[sessionID])
	// Atomic publication of the queued state: meta (first writer wins — a
	// rebuild path may have re-created it) plus the queued event, mirroring
	// the Redis admit script.
	if _, ok := m.meta[entry.RunID]; !ok {
		m.meta[entry.RunID] = RunMeta{
			SessionID: entry.SessionID,
			UserID:    entry.UserID,
			Status:    StatusQueued,
			Model:     entry.Model,
			CreatedAt: entry.EnqueuedAt.Format(time.RFC3339),
		}
	}
	m.expire[entry.RunID] = m.now().Add(RunKeyTTL)
	seq := len(m.streams[entry.RunID]) + 1
	m.streams[entry.RunID] = append(m.streams[entry.RunID], memEntry{
		Entry:   Entry{ID: strconv.Itoa(seq) + "-1", Event: stream.QueuedEvent(entry.RunID, pos)},
		expires: m.now().Add(RunKeyTTL),
	})
	return AdmitResult{Outcome: AdmitQueued, Position: pos}, nil
}

// PumpHead implements Store: pop head + claim transfer in one critical
// section; a held claim (a racing admission won it) leaves the head enqueued.
func (m *MemStore) PumpHead(_ context.Context, sessionID string) (QueueEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[sessionID]
	if len(q) == 0 {
		return QueueEntry{}, false, nil
	}
	if m.claimHeldLocked(sessionID) {
		return QueueEntry{}, false, nil
	}
	head := q[0]
	m.queues[sessionID] = q[1:]
	if len(m.queues[sessionID]) == 0 {
		delete(m.queues, sessionID) // mirror Redis's auto-delete of empty lists
	}
	m.claims[sessionID] = head.RunID
	m.claimExp[sessionID] = m.now().Add(SessionClaimTTL)
	return head, true, nil
}

// RequeueHead implements Store: unshift the entry and release the claim when
// it still points at the entry's run (one critical section).
func (m *MemStore) RequeueHead(_ context.Context, sessionID string, entry QueueEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[sessionID] = append([]QueueEntry{entry}, m.queues[sessionID]...)
	if m.claims[sessionID] == entry.RunID {
		delete(m.claims, sessionID)
		delete(m.claimExp, sessionID)
	}
	return nil
}

// DequeueMessage implements Store (remove by run id; claim untouched).
func (m *MemStore) DequeueMessage(_ context.Context, sessionID, runID string) (QueueEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[sessionID]
	for i, e := range q {
		if e.RunID == runID {
			m.queues[sessionID] = append(q[:i:i], q[i+1:]...)
			if len(m.queues[sessionID]) == 0 {
				delete(m.queues, sessionID)
			}
			return e, true, nil
		}
	}
	return QueueEntry{}, false, nil
}

// QueueLen implements Store.
func (m *MemStore) QueueLen(_ context.Context, sessionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queues[sessionID]), nil
}

// QueueEntries implements Store (enqueue order).
func (m *MemStore) QueueEntries(_ context.Context, sessionID string) ([]QueueEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[sessionID]
	if len(q) == 0 {
		return nil, nil
	}
	out := make([]QueueEntry, len(q))
	copy(out, q)
	return out, nil
}

// TouchQueued implements Store: re-arm the run-key family TTL only (no
// heartbeat, no claim), mirroring the Redis EXPIRE pair.
func (m *MemStore) TouchQueued(_ context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.expire[runID]; ok && !m.expiredLocked(runID) {
		m.expire[runID] = m.now().Add(RunKeyTTL)
	}
	return nil
}

// ClearQueue implements Store: drop every entry and terminate each dropped
// run as cancelled with the terminal retain window — subscribers tailing
// those runs observe a terminal status and close instead of waiting out the
// meta TTL.
func (m *MemStore) ClearQueue(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.queues[sessionID]
	delete(m.queues, sessionID)
	for _, e := range entries {
		if meta, ok := m.meta[e.RunID]; ok {
			meta.Status = StatusCancelled
			m.meta[e.RunID] = meta
			m.expire[e.RunID] = m.now().Add(RetainAfterTerminal)
		}
		delete(m.alive, e.RunID)
		delete(m.cancels, e.RunID)
	}
	return nil
}

// QueuedSessions implements Store (sorted for deterministic tests).
func (m *MemStore) QueuedSessions(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.queues))
	for sid := range m.queues {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out, nil
}

// Alive implements Store.
func (m *MemStore) Alive(_ context.Context, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.alive[runID]
	return ok && m.now().Before(exp), nil
}

// SetCancelFlag implements Store.
func (m *MemStore) SetCancelFlag(_ context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancels[runID] = true
	return nil
}

// TakeCancelFlag implements Store (read-and-clear).
func (m *MemStore) TakeCancelFlag(_ context.Context, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	flagged := m.cancels[runID]
	delete(m.cancels, runID)
	return flagged, nil
}

// Retire implements Store: heartbeat/cancel keys drop now, event log and meta
// move to the retain-window expiry.
func (m *MemStore) Retire(_ context.Context, runID string, retain time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.alive, runID)
	delete(m.cancels, runID)
	if _, ok := m.meta[runID]; ok {
		m.expire[runID] = m.now().Add(retain)
	}
	return nil
}
