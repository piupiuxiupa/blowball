package llmraw

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/model"
)

// newTestBuffer starts a miniredis and returns a *redis.Store-style Buffer
// backed by it (reusing the production store via the real client).
func newTestBuffer(t *testing.T) *redisStoreBuffer {
	t.Helper()
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	return &redisStoreBuffer{cli: cli}
}

// redisStoreBuffer adapts a raw *redis.Client to the Buffer port using the
// same commands the production redis.Store methods issue.
type redisStoreBuffer struct {
	cli *redis.Client
}

func (b *redisStoreBuffer) PushRawLog(ctx context.Context, raw []byte) error {
	return b.cli.RPush(ctx, "llm_raw:buffer", raw).Err()
}

func (b *redisStoreBuffer) PopRawLogs(ctx context.Context, count int64) ([][]byte, error) {
	res, err := b.cli.LPopCount(ctx, "llm_raw:buffer", int(count)).Result()
	if err != nil {
		if err.Error() == "redis: nil" {
			return nil, nil
		}
		return nil, err
	}
	out := make([][]byte, 0, len(res))
	for i := range res {
		out = append(out, []byte(res[i]))
	}
	return out, nil
}

func (b *redisStoreBuffer) LenRawLogs(ctx context.Context) (int64, error) {
	return b.cli.LLen(ctx, "llm_raw:buffer").Result()
}

// fakeRawLogStore records inserted rows.
type fakeRawLogStore struct {
	mu   sync.Mutex
	rows []model.LLMRawLog
	fail bool
	// attempts counts every AppendRawLogs call, failed or not, so tests can
	// wait for a failed insert ATTEMPT to have completed before healing the
	// store (buffer-empty alone cannot distinguish "dropped" from "popped,
	// insert in flight").
	attempts int
}

func (f *fakeRawLogStore) AppendRawLogs(_ context.Context, logs []model.LLMRawLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fail {
		return assert.AnError
	}
	f.rows = append(f.rows, logs...)
	return nil
}

func (f *fakeRawLogStore) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeRawLogStore) snapshot() []model.LLMRawLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.LLMRawLog, len(f.rows))
	copy(out, f.rows)
	return out
}

func captureRec(callID string, seq int, kind string) agent.RawCaptureRecord {
	return agent.RawCaptureRecord{
		CallID:    callID,
		Seq:       seq,
		TraceID:   "trace-1",
		SessionID: "sess-1",
		UserID:    "user-1",
		Agent:     "Confucius",
		Kind:      kind,
		Model:     "gpt-test",
		Raw:       `{"k":` + kind + `}`,
		MsgTime:   time.Now().UTC(),
	}
}

func TestSinkFlusherPipeline_PairedRowsArriveOrdered(t *testing.T) {
	buf := newTestBuffer(t)
	store := &fakeRawLogStore{}
	notify := make(chan struct{}, 1)
	sink := NewSink(buf, notify)
	flusher := NewFlusher(buf, store, notify)
	flusher.Start()
	defer flusher.Close()

	ctx := context.Background()
	// Two LLM calls' worth of paired records, pushed in call order.
	for _, rec := range []agent.RawCaptureRecord{
		captureRec("call-1", 1, model.RawKindRequest),
		captureRec("call-1", 1, model.RawKindResponse),
		captureRec("call-2", 2, model.RawKindRequest),
		captureRec("call-2", 2, model.RawKindError),
	} {
		sink.Capture(ctx, rec)
	}

	// Below the batch threshold the ticker owns flushing; the final drain in
	// Close must land all four rows in FIFO order regardless.
	require.Eventually(t, func() bool {
		return len(store.snapshot()) == 4
	}, flushInterval+2*time.Second, 20*time.Millisecond)

	rows := store.snapshot()
	require.Len(t, rows, 4)
	assert.Equal(t, "call-1", rows[0].CallID)
	assert.Equal(t, model.RawKindRequest, rows[0].Kind)
	assert.Equal(t, model.RawKindResponse, rows[1].Kind)
	assert.Equal(t, "call-2", rows[2].CallID)
	assert.Equal(t, model.RawKindError, rows[3].Kind)
	assert.Equal(t, "Confucius", rows[0].Agent)
	assert.Equal(t, "sess-1", rows[0].SessionID)
}

func TestFlusher_BatchThresholdEarlyFlush(t *testing.T) {
	buf := newTestBuffer(t)
	notify := make(chan struct{}, 1)
	sink := NewSink(buf, notify)
	// No Flusher.Start: drive the threshold logic by hand — pushing
	// flushBatchSize records must leave the notify channel signalled.
	for i := 0; i < flushBatchSize; i++ {
		sink.Capture(context.Background(), captureRec("call-x", i+1, model.RawKindRequest))
	}
	select {
	case <-notify:
		// expected: the sink nudged the flusher
	default:
		t.Fatal("sink must nudge the flusher once the queue crosses the batch threshold")
	}

	n, err := buf.LenRawLogs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(flushBatchSize), n)
}

func TestFlusher_FinalFlushOnClose(t *testing.T) {
	buf := newTestBuffer(t)
	store := &fakeRawLogStore{}
	// nil notify channel: the nudge is optional and must not break anything.
	flusher := NewFlusher(buf, store, nil)
	flusher.Start()

	// Records land after the loop started but without any ticker firing yet.
	for i := 0; i < 3; i++ {
		require.NoError(t, buf.PushRawLog(context.Background(), []byte(`{"call_id":"c","seq":1,"kind":"request","agent":"Liang"}`)))
	}

	flusher.Close() // must drain the buffer even though the ticker never fired
	assert.Len(t, store.snapshot(), 3)

	// Close is idempotent.
	flusher.Close()
	assert.Len(t, store.snapshot(), 3)
}

func TestFlusher_InsertFailureBackoffAndDropsBatch(t *testing.T) {
	buf := newTestBuffer(t)
	store := &fakeRawLogStore{fail: true}
	notify := make(chan struct{}, 1)
	sink := NewSink(buf, notify)
	flusher := NewFlusher(buf, store, notify)
	flusher.Start()
	defer flusher.Close()

	sink.Capture(context.Background(), captureRec("c", 1, model.RawKindRequest))

	// The batch fails on the first tick and is dropped (at-most-once); the
	// flusher backs off rather than tearing down, so the buffer drains to 0.
	// Wait for the FAILED INSERT ATTEMPT to have completed — not just for the
	// buffer to empty. The pop (LPOP) empties the buffer before the insert
	// runs, so a buffer-only wait can pass while the failing AppendRawLogs is
	// still in flight; healing at that moment lets the "dropped" batch insert
	// late and the final row count comes out one high (a -race flake).
	require.Eventually(t, func() bool {
		n, err := buf.LenRawLogs(context.Background())
		return err == nil && n == 0 && store.attemptCount() >= 1
	}, flushInterval+2*time.Second, 20*time.Millisecond, "failed batch must be dropped, not re-queued forever")

	// Recovery: once the store heals, later records flow through. Verified
	// deterministically via the synchronous close-drain (the nudge path is
	// separately covered by TestFlusher_BatchThresholdEarlyFlush).
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	for i := 0; i < flushBatchSize; i++ {
		sink.Capture(context.Background(), captureRec("c2", i+2, model.RawKindRequest))
	}
	flusher.Close()
	assert.Len(t, store.snapshot(), flushBatchSize, "healed store must accept the post-recovery records (the failed c1 batch stays dropped)")
}

func TestFlusher_MalformedRecordDroppedNotBatch(t *testing.T) {
	buf := newTestBuffer(t)
	store := &fakeRawLogStore{}
	flusher := NewFlusher(buf, store, nil)

	require.NoError(t, buf.PushRawLog(context.Background(), []byte(`{not-json`)))
	require.NoError(t, buf.PushRawLog(context.Background(), []byte(`{"call_id":"ok","seq":1,"kind":"request"}`)))

	// Direct drain via Close exercises flushAll's decode isolation.
	flusher.Close()
	rows := store.snapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, "ok", rows[0].CallID)
}

func TestSink_PushFailureDropsAndNeverPanics(t *testing.T) {
	// A buffer whose Push always fails: Capture logs and drops, never panics,
	// never blocks the caller.
	buf := &failingBuffer{}
	sink := NewSink(buf, make(chan struct{}, 1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.Capture(context.Background(), captureRec("call-1", 1, model.RawKindRequest))
	}()
	select {
	case <-done:
		// expected
	case <-time.After(2 * pushTimeout):
		t.Fatal("Capture must stay bounded when the buffer is down")
	}
}

type failingBuffer struct{}

func (failingBuffer) PushRawLog(context.Context, []byte) error { return assert.AnError }
func (failingBuffer) PopRawLogs(context.Context, int64) ([][]byte, error) {
	return nil, nil
}
func (failingBuffer) LenRawLogs(context.Context) (int64, error) { return 0, nil }
