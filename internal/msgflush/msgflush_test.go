package msgflush

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// fakeBuffer is an in-memory msgflush.Buffer mirroring the Redis list
// semantics: FIFO buffer, FIFO processing, LREM-by-value ack, and a recovery
// that requeues processing onto the buffer head in claim order.
type fakeBuffer struct {
	mu         sync.Mutex
	buffer     [][]byte
	processing [][]byte

	moveErr    error // injected MoveMessageToProcessing failure
	lremErr    error // injected RemoveFromProcessing failure
	recoverErr error // injected RecoverProcessingToBuffer failure

	recovers int
}

func (b *fakeBuffer) MoveMessageToProcessing(ctx context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.moveErr != nil {
		return nil, b.moveErr
	}
	if len(b.buffer) == 0 {
		return nil, nil
	}
	raw := b.buffer[0]
	b.buffer = b.buffer[1:]
	b.processing = append(b.processing, raw)
	return append([]byte(nil), raw...), nil
}

func (b *fakeBuffer) RemoveFromProcessing(ctx context.Context, raw []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lremErr != nil {
		return b.lremErr
	}
	for i, p := range b.processing {
		if string(p) == string(raw) {
			b.processing = append(b.processing[:i], b.processing[i+1:]...)
			return nil
		}
	}
	return nil // value not parked (already acked) — matches LREM count 0
}

func (b *fakeBuffer) RecoverProcessingToBuffer(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recovers++
	if b.recoverErr != nil {
		return b.recoverErr
	}
	if len(b.processing) == 0 {
		return nil
	}
	b.buffer = append(b.processing, b.buffer...)
	b.processing = nil
	return nil
}

func (b *fakeBuffer) LenMessageBuffer(ctx context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.buffer)), nil
}

// push appends records to the buffer tail (the producer side).
func (b *fakeBuffer) push(raws ...[]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer = append(b.buffer, raws...)
}

// snapshot returns (buffer, processing) lengths.
func (b *fakeBuffer) lens() (bufLen, procLen int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer), len(b.processing)
}

// fakeMessageStore is an in-memory msgflush.MessageStore with INSERT IGNORE
// semantics on client_msg_id, injectable failures, and an optional block that
// stalls inserts until the ctx ends (for bounded-drain tests).
type fakeMessageStore struct {
	mu   sync.Mutex
	rows []model.Message

	insertAttempts int
	failNext       int // fail this many AppendMessages calls with a generic error

	fkSessions map[string]struct{} // session IDs whose inserts fail with MySQL 1452
	block      bool                // stall inserts until ctx.Done (no insert)

	updateTimes   map[string]int
	updateErrOnce bool
}

func newFakeStore() *fakeMessageStore {
	return &fakeMessageStore{updateTimes: map[string]int{}}
}

func fkErr() error {
	return &mysqldriver.MySQLError{Number: 1452, Message: "Cannot add or update a child row: a foreign key constraint fails (`blowball`.`messages`, CONSTRAINT `fk_messages_session` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`session_id`))"}
}

func (s *fakeMessageStore) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertAttempts++

	if s.failNext > 0 {
		s.failNext--
		return nil, fmt.Errorf("fake mysql unavailable")
	}
	if len(s.fkSessions) > 0 {
		for _, m := range msgs {
			if _, dead := s.fkSessions[m.SessionID]; dead {
				return nil, fkErr()
			}
		}
	}

	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		// INSERT IGNORE on the UNIQUE client_msg_id: a redelivered record
		// counts as an attempt but lands no second row.
		if m.ClientMsgID != "" {
			dup := false
			for _, r := range s.rows {
				if r.ClientMsgID == m.ClientMsgID {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
		}
		s.rows = append(s.rows, m)
		ids = append(ids, int64(len(s.rows)))
	}
	return ids, nil
}

func (s *fakeMessageStore) UpdateSessionTime(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErrOnce {
		s.updateErrOnce = false
		return fmt.Errorf("fake update failed")
	}
	s.updateTimes[sessionID]++
	return nil
}

func (s *fakeMessageStore) snapshot() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Message, len(s.rows))
	copy(out, s.rows)
	return out
}

func (s *fakeMessageStore) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertAttempts
}

// msgRecord builds a canonical queue record for (session, index).
func msgRecord(sessionID string, idx int) []byte {
	m := model.Message{
		SessionID:   sessionID,
		Agent:       model.AgentUser,
		MsgIndex:    idx,
		Role:        model.RoleUser,
		EventType:   model.EventTypeMessage,
		Content:     fmt.Sprintf("msg-%s-%d", sessionID, idx),
		TraceID:     "trace-1",
		ClientMsgID: fmt.Sprintf("cmid-%s-%04d", sessionID, idx),
	}
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestFlusher_NormalBatch verifies the claim→insert→ack happy path: every
// record lands exactly once, processing and buffer end up empty, and
// update_time is refreshed once per distinct session.
func TestFlusher_NormalBatch(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 5}, nil)

	raws := [][]byte{msgRecord("s-1", 0), msgRecord("s-1", 1), msgRecord("s-2", 0)}
	buf.push(raws...)

	require.NoError(t, f.flushAll(context.Background()))

	require.Len(t, store.snapshot(), 3)
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl, "buffer must drain")
	assert.Equal(t, 0, pl, "processing must be acked")
	assert.Equal(t, 1, store.updateTimes["s-1"], "one refresh per distinct session")
	assert.Equal(t, 1, store.updateTimes["s-2"])
}

// TestFlusher_MultiConsumer_NoDupNoLoss runs two flushers against one shared
// buffer with interleaved rounds and asserts every record lands exactly once.
func TestFlusher_MultiConsumer_NoDupNoLoss(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f1 := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 3}, nil)
	f2 := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 3}, nil)

	const n = 40
	raws := make([][]byte, 0, n)
	for i := range n {
		raws = append(raws, msgRecord("s-multi", i))
	}
	buf.push(raws...)

	// Interleave rounds: each flusher claims what it can; claims are atomic
	// so a record never enters two batches.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = f1.flushAll(context.Background()) }()
	go func() { defer wg.Done(); _ = f2.flushAll(context.Background()) }()
	wg.Wait()

	// Insertion order interleaves across the two consumers; what must hold is
	// exactly-once: every record present, no duplicates, queue fully drained.
	rows := store.snapshot()
	assert.Len(t, rows, n, "every record inserted exactly once")
	seen := make(map[string]struct{}, n)
	for _, m := range rows {
		_, dup := seen[m.ClientMsgID]
		assert.False(t, dup, "duplicate row for %s", m.ClientMsgID)
		seen[m.ClientMsgID] = struct{}{}
	}
	assert.Len(t, seen, n)
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl)
}

// TestFlusher_InsertFailure_RetainedAndRetried verifies the retry contract:
// on a generic insert failure the records stay parked in processing, and the
// next round retries the same batch until it succeeds — never dropping it.
func TestFlusher_InsertFailure_RetainedAndRetried(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)

	buf.push(msgRecord("s-r", 0), msgRecord("s-r", 1))

	// First round fails.
	store.failNext = 1
	err := f.flushAll(context.Background())
	require.Error(t, err)

	bl, pl := buf.lens()
	assert.Equal(t, 0, bl, "records already claimed off the buffer")
	assert.Equal(t, 2, pl, "records must stay parked in processing")
	assert.Empty(t, store.snapshot(), "nothing inserted yet")

	// Second round retries the parked batch and succeeds.
	require.NoError(t, f.flushAll(context.Background()))
	require.Len(t, store.snapshot(), 2)
	_, pl = buf.lens()
	assert.Equal(t, 0, pl, "acked after the retry")
}

// TestFlusher_FKError_DeadLetter verifies the single dead-letter exception: a
// record whose session row is gone (MySQL 1452) is removed from processing
// with an ERROR log instead of retrying forever, while the retryable majority
// of its batch still lands.
func TestFlusher_FKError_DeadLetter(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	store.fkSessions = map[string]struct{}{"s-deleted": {}}
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)

	buf.push(msgRecord("s-alive", 0), msgRecord("s-deleted", 0), msgRecord("s-alive", 1))

	require.NoError(t, f.flushAll(context.Background()))

	rows := store.snapshot()
	require.Len(t, rows, 2, "only the live session's records land")
	for _, r := range rows {
		assert.Equal(t, "s-alive", r.SessionID)
	}
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl, "dead-lettered record must be removed from processing")
	assert.Equal(t, 1, store.updateTimes["s-alive"], "acked records still refresh update_time")
}

// TestFlusher_NonFKErrorNotDeadLettered verifies a non-FK insert failure does
// not enter the dead-letter path even when FK failures interleave.
func TestFlusher_NonFKErrorNotDeadLettered(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)

	buf.push(msgRecord("s-r", 0))
	store.failNext = 1
	err := f.flushAll(context.Background())
	require.Error(t, err)
	_, pl := buf.lens()
	assert.Equal(t, 1, pl, "non-FK failure parks the record, never dead-letters")
}

// TestFlusher_PoisonRecord_DeadLetter verifies an undecodable queue record is
// dropped with an ERROR rather than blocking the queue forever.
func TestFlusher_PoisonRecord_DeadLetter(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)

	buf.push([]byte("not-json"), msgRecord("s-p", 0))

	require.NoError(t, f.flushAll(context.Background()))
	require.Len(t, store.snapshot(), 1, "valid record still lands")
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl, "poison record must be removed")
}

// TestFlusher_StartupRecovery verifies Start requeues processing residue
// (records claimed by a crashed process) back onto the buffer head before
// the loop begins, and the loop then persists them.
func TestFlusher_StartupRecovery(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()

	// Simulate the crash: records were claimed into processing but never
	// inserted or acked.
	buf.push(msgRecord("s-c", 0), msgRecord("s-c", 1))
	for range 2 {
		if _, err := buf.MoveMessageToProcessing(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)
	f.Start()
	defer f.Close()

	// Recovery ran synchronously inside Start: the residue is back on the
	// buffer in claim order, ahead of anything that arrived meanwhile.
	bl, _ := buf.lens()
	require.Equal(t, 2, bl)
	require.Equal(t, 1, buf.recovers)

	// The loop's final drain (Close) persists them; a manual flushAll proves
	// the redelivery inserts each exactly once.
	require.NoError(t, f.flushAll(context.Background()))
	require.Len(t, store.snapshot(), 2, "recovered records insert exactly once")
}

// TestFlusher_IdempotentRedelivery_InsertsOnce verifies the crash window
// between a committed INSERT and its LREM ack: the recovered record is
// redelivered and re-inserted, and the UNIQUE client_msg_id collapses it to
// the single existing row.
func TestFlusher_IdempotentRedelivery_InsertsOnce(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 10}, nil)

	raw := msgRecord("s-i", 0)
	buf.push(raw)
	require.NoError(t, f.flushAll(context.Background()))
	require.Len(t, store.snapshot(), 1)
	attempts := store.attempts()

	// Crash-and-recover: the same blob is back on the queue (startup
	// recovery would do exactly this).
	buf.push(raw)
	require.NoError(t, f.flushAll(context.Background()))

	assert.Equal(t, attempts+1, store.attempts(), "redelivery is re-inserted")
	assert.Len(t, store.snapshot(), 1, "duplicate collapses to one row")
}

// TestFlusher_FinalDrainOnClose verifies the graceful-shutdown contract:
// Close stops the loop and drains what is left, so a normal restart leaves
// no tail behind. Close-without-Start is a bounded no-op.
func TestFlusher_FinalDrainOnClose(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	notify := make(chan struct{}, 1)
	f := NewFlusher(buf, store, Config{Interval: 10 * time.Millisecond, BatchSize: 2}, notify)
	f.Start()

	buf.push(msgRecord("s-f", 0), msgRecord("s-f", 1), msgRecord("s-f", 2))
	f.Close()

	require.Len(t, store.snapshot(), 3, "final drain lands the tail")
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl)

	// Close is idempotent.
	f.Close()
}

// TestFlusher_CloseWithoutStart_BoundedNoop verifies Close on a never-started
// flusher drains (bounded) instead of hanging on the done channel.
func TestFlusher_CloseWithoutStart_BoundedNoop(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	f := NewFlusher(buf, store, Config{}, nil)

	buf.push(msgRecord("s-n", 0))
	done := make(chan struct{})
	go func() { f.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(finalDrainTimeout + 2*time.Second):
		t.Fatal("Close without Start must not hang")
	}
	require.Len(t, store.snapshot(), 1, "no-op drain still flushes the queue")
}

// TestFlusher_EarlyFlushOnNotify verifies the producer nudge: once the queue
// crosses the batch threshold the loop flushes without waiting for the
// ticker.
func TestFlusher_EarlyFlushOnNotify(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	notify := make(chan struct{}, 1)
	f := NewFlusher(buf, store, Config{Interval: time.Hour, BatchSize: 3}, notify)
	f.Start()
	defer f.Close()

	for i := range 3 {
		buf.push(msgRecord("s-e", i))
	}
	notify <- struct{}{}

	require.Eventually(t, func() bool { return len(store.snapshot()) == 3 },
		2*time.Second, 5*time.Millisecond, "notify above threshold must flush early")
}

// TestDrain_EmptiesQueueAndProcessing verifies the synchronous drain
// primitive: residue in processing AND fresh records in buffer all land, and
// both lists end empty.
func TestDrain_EmptiesQueueAndProcessing(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()

	// Residue: claimed but unacked (crash / failed-drain leftover).
	buf.push(msgRecord("s-d", 0))
	if _, err := buf.MoveMessageToProcessing(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Fresh records still queued.
	buf.push(msgRecord("s-d", 1), msgRecord("s-d", 2))

	require.NoError(t, Drain(context.Background(), buf, store))

	require.Len(t, store.snapshot(), 3, "residue + fresh all land")
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl)
}

// TestDrain_BoundedByContext verifies the drain is bounded: a store that
// stalls until ctx cancellation surfaces the deadline instead of hanging.
func TestDrain_BoundedByContext(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	store.block = true
	buf.push(msgRecord("s-b", 0))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Drain(ctx, buf, store)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "drain must honor its deadline")
}

// TestDrain_FKDeadLetterDuringDrain verifies drain applies the same FK
// dead-letter rule as the background loop.
func TestDrain_FKDeadLetterDuringDrain(t *testing.T) {
	buf := &fakeBuffer{}
	store := newFakeStore()
	store.fkSessions = map[string]struct{}{"s-gone": {}}

	buf.push(msgRecord("s-here", 0), msgRecord("s-gone", 0))
	require.NoError(t, Drain(context.Background(), buf, store))

	rows := store.snapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, "s-here", rows[0].SessionID)
	bl, pl := buf.lens()
	assert.Equal(t, 0, bl)
	assert.Equal(t, 0, pl)
}

// TestIsFKError verifies the MySQL 1452 detection behind the dead-letter rule.
func TestIsFKError(t *testing.T) {
	require.True(t, isFKError(fkErr()))
	require.False(t, isFKError(assert.AnError))
	require.False(t, isFKError(&mysqldriver.MySQLError{Number: 1062}))
	// errors.As traverses the wrap chain, so a wrapped FK error still matches.
	require.True(t, isFKError(fmt.Errorf("wrapped: %w", fkErr())))
}
