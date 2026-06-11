package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushRecorder is an http.ResponseWriter that buffers all writes and counts
// Flush calls. It implements http.Flusher so WriteSSE accepts it. Header writes
// are captured so tests can assert SSE headers were set.
type flushRecorder struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	header    http.Header
	status    int
	flushes   int64
	written   int64
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: make(http.Header)}
}

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt64(&f.written, int64(len(b)))
	return f.buf.Write(b)
}

func (f *flushRecorder) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

func (f *flushRecorder) Flush() { atomic.AddInt64(&f.flushes, 1) }

func (f *flushRecorder) Bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy so the caller can safely read after the writer is done.
	out := make([]byte, f.buf.Len())
	copy(out, f.buf.Bytes())
	return out
}

func (f *flushRecorder) Flushes() int64 { return atomic.LoadInt64(&f.flushes) }

// nonFlusherWriter intentionally does NOT implement http.Flusher.
type nonFlusherWriter struct{}

func (nonFlusherWriter) Header() http.Header        { return http.Header{} }
func (nonFlusherWriter) Write([]byte) (int, error)  { return 0, nil }
func (nonFlusherWriter) WriteHeader(int)            {}

// TestWriteSSE_RejectsNonFlusher verifies the writer must implement Flusher.
func TestWriteSSE_RejectsNonFlusher(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	err := WriteSSE(context.Background(), nonFlusherWriter{}, h)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Flusher")
}

// TestWriteSSE_HeadersSet verifies all required SSE headers are present before
// the first write and that the status is 200.
func TestWriteSSE_HeadersSet(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	rec := newFlushRecorder()
	done := make(chan error, 1)
	go func() { done <- WriteSSE(context.Background(), rec, h) }()

	require.True(t, h.Send(TokenEvent(AgentConfuse, "hi")))
	time.Sleep(30 * time.Millisecond)
	h.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return after hub close")
	}

	assert.Equal(t, http.StatusOK, rec.status)
	assert.Equal(t, "text/event-stream", rec.header.Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.header.Get("Connection"))
	assert.Equal(t, "no", rec.header.Get("X-Accel-Buffering"))
}

// TestWriteSSE_EventFormat verifies the wire format produced for a token event
// and that the JSON payload round-trips to the original StreamEvent.
func TestWriteSSE_EventFormat(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	rec := newFlushRecorder()
	done := make(chan error, 1)
	go func() { done <- WriteSSE(context.Background(), rec, h) }()

	orig := TokenEvent(AgentConfuse, "hello world")
	require.True(t, h.Send(orig))

	// Drain + close so WriteSSE returns and we can inspect the buffer.
	require.Eventually(t, func() bool { return atomic.LoadInt64(&rec.written) > 0 },
		time.Second, 5*time.Millisecond)
	h.Close()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return")
	}

	out := string(rec.Bytes())
	require.NotEmpty(t, out)

	// Each event ends with a blank line.
	require.True(t, strings.HasSuffix(out, "\n\n"),
		"event must end with blank line; got %q", out)

	// Header line present and correct.
	require.True(t, strings.HasPrefix(out, "event: token\n"),
		"first line must be 'event: token'; got %q", out)

	// Extract the data line and parse the JSON back.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var dataLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			dataLine = strings.TrimPrefix(l, "data: ")
			break
		}
	}
	require.NotEmpty(t, dataLine, "expected a data: line, got %q", out)

	var got StreamEvent
	require.NoError(t, json.Unmarshal([]byte(dataLine), &got))
	assert.Equal(t, orig, got)
}

// TestWriteSSE_MultipleEvents verifies ordering and per-event flushing.
func TestWriteSSE_MultipleEvents(t *testing.T) {
	h := NewHub(32)
	defer h.Close()

	rec := newFlushRecorder()
	done := make(chan error, 1)
	go func() { done <- WriteSSE(context.Background(), rec, h) }()

	events := []StreamEvent{
		AgentStartEvent(AgentConfuse),
		TokenEvent(AgentConfuse, "a"),
		TokenEvent(AgentConfuse, "b"),
		AgentEndEvent(AgentConfuse),
		DoneEvent(nil),
	}
	for _, e := range events {
		require.True(t, h.Send(e))
	}

	require.Eventually(t, func() bool {
		return strings.Count(string(rec.Bytes()), "event:") >= len(events)
	}, time.Second, 5*time.Millisecond)
	h.Close()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return")
	}

	// Each event must be flushed at least once; WriteSSE also flushes the header.
	assert.GreaterOrEqual(t, rec.Flushes(), int64(len(events)),
		"expected at least one flush per event; got %d", rec.Flushes())
	assert.Equal(t, len(events), strings.Count(string(rec.Bytes()), "event:"))
}

// TestWriteSSE_ContextCancellation verifies that cancelling the context causes
// WriteSSE to return ctx.Err() promptly, even if the hub is still open.
func TestWriteSSE_ContextCancellation(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- WriteSSE(ctx, rec, h) }()

	// Let it get into the receive loop, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled),
			"expected context.Canceled, got %v", err)
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return after ctx cancel")
	}
}

// TestHub_Close_StopsWriteSSE verifies that closing the hub terminates WriteSSE
// with a nil error (clean shutdown) once buffered events are drained.
func TestHub_Close_StopsWriteSSE(t *testing.T) {
	h := NewHub(8)
	rec := newFlushRecorder()

	done := make(chan error, 1)
	go func() { done <- WriteSSE(context.Background(), rec, h) }()

	require.True(t, h.Send(TokenEvent(AgentConfuse, "last")))
	time.Sleep(20 * time.Millisecond)
	h.Close()

	select {
	case err := <-done:
		require.NoError(t, err, "clean shutdown after hub close should return nil")
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return after hub close")
	}

	assert.Contains(t, string(rec.Bytes()), "event: token")
}

// TestWriteSSE_BlockedConsumerRespectsCtx covers the path where the consumer
// never drains: WriteSSE must still exit when ctx is cancelled.
func TestWriteSSE_BlockedConsumerRespectsCtx(t *testing.T) {
	h := NewHub(1) // tiny buffer so producers backpressure quickly
	rec := newFlushRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := WriteSSE(ctx, rec, h)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.Less(t, elapsed, time.Second, "must return promptly after ctx deadline")
}

// TestWriteSSE_DoneEventJSON verifies the done event serializes usage nested
// object correctly (common case for end-of-stream).
func TestWriteSSE_DoneEventJSON(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	rec := newFlushRecorder()
	done := make(chan error, 1)
	go func() { done <- WriteSSE(context.Background(), rec, h) }()

	usage := map[string]any{
		"total_tokens": 42,
		"agents": map[string]any{
			AgentConfuse: map[string]any{"prompt_tokens": 10, "completion_tokens": 32},
		},
	}
	require.True(t, h.Send(DoneEvent(usage)))

	require.Eventually(t, func() bool {
		return strings.Contains(string(rec.Bytes()), "done")
	}, time.Second, 5*time.Millisecond)
	h.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WriteSSE did not return")
	}

	out := string(rec.Bytes())
	assert.Contains(t, out, "event: done")
	assert.Contains(t, out, fmt.Sprintf(`"total_tokens":42`))
	assert.Contains(t, out, `"prompt_tokens":10`)
}
