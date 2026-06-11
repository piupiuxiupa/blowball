package stream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewHub_DefaultBufferSize ensures NewHub(0) falls back to the default.
func TestNewHub_DefaultBufferSize(t *testing.T) {
	h := NewHub(0)
	require.Equal(t, DefaultHubBufferSize, cap(h.ch))
	require.NotZero(t, DefaultHubBufferSize)
	h.Close()
}

// TestNewHub_NegativeBufferSize falls back to default rather than panicking.
func TestNewHub_NegativeBufferSize(t *testing.T) {
	h := NewHub(-1)
	require.Equal(t, DefaultHubBufferSize, cap(h.ch))
	h.Close()
}

// TestSendAndReceive sends an event and verifies it is received unchanged on
// the Events() channel.
func TestSendAndReceive(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	want := TokenEvent(AgentConfuse, "hello")
	require.True(t, h.Send(want))

	select {
	case got := <-h.Events():
		assert.Equal(t, want, got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestSend_AfterClose returns false and does not block once the hub is closed.
func TestSend_AfterClose(t *testing.T) {
	h := NewHub(8)
	h.Close()

	ok := h.Send(TokenEvent(AgentConfuse, "x"))
	assert.False(t, ok, "Send after Close must return false")
}

// TestSendCtx_RespectsContextCancel verifies a cancelled context aborts SendCtx.
func TestSendCtx_RespectsContextCancel(t *testing.T) {
	h := NewHub(1)
	defer h.Close()

	// Fill the buffer so the next send must block.
	require.True(t, h.Send(TokenEvent(AgentConfuse, "fill")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	start := time.Now()
	ok := h.SendCtx(ctx, TokenEvent(AgentConfuse, "drop"))
	elapsed := time.Since(start)

	assert.False(t, ok, "SendCtx with cancelled ctx must return false")
	assert.Less(t, elapsed, 200*time.Millisecond, "SendCtx must return promptly on ctx cancel")
}

// TestSendCtx_RunsToCompletion verifies a successful SendCtx returns true.
func TestSendCtx_RunsToCompletion(t *testing.T) {
	h := NewHub(8)
	defer h.Close()

	ctx := context.Background()
	require.True(t, h.SendCtx(ctx, TokenEvent(AgentConfuse, "ok")))

	select {
	case got := <-h.Events():
		assert.Equal(t, "ok", got.Content)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestSendCtx_HubClosed verifies SendCtx returns false when hub closes mid-send.
func TestSendCtx_HubClosed(t *testing.T) {
	h := NewHub(1)
	// Fill buffer so SendCtx will block.
	require.True(t, h.Send(TokenEvent(AgentConfuse, "fill")))

	go func() {
		time.Sleep(20 * time.Millisecond)
		h.Close()
	}()

	ctx := context.Background()
	start := time.Now()
	ok := h.SendCtx(ctx, TokenEvent(AgentConfuse, "drop"))
	elapsed := time.Since(start)
	assert.False(t, ok, "SendCtx must return false when hub closes")
	assert.Less(t, elapsed, time.Second)
}

// TestHub_Close_IsIdempotent verifies repeated Close calls do not panic.
func TestHub_Close_IsIdempotent(t *testing.T) {
	h := NewHub(8)

	require.NotPanics(t, func() {
		h.Close()
		h.Close()
		h.Close()
	})

	// Done() channel is closed exactly once and remains closed.
	<-h.Done() // does not block
	select {
	case <-h.Done():
	default:
		t.Fatal("Done() should remain closed")
	}
}

// TestHub_Close_DrainsAndPreservesBuffered verifies that buffered events remain
// receivable from Events() after Close, and that a non-blocking drain picks
// them up. Per the Hub doc comment the events channel itself is NOT closed, so
// consumers must drain non-blockingly and observe Done() to know they're done.
func TestHub_Close_DrainsAndPreservesBuffered(t *testing.T) {
	h := NewHub(8)
	require.True(t, h.Send(TokenEvent(AgentConfuse, "a")))
	require.True(t, h.Send(TokenEvent(AgentConfuse, "b")))
	h.Close()

	// Done() is signaled immediately.
	select {
	case <-h.Done():
	default:
		t.Fatal("Done() should be closed after Close")
	}

	// Buffered events are still readable via a non-blocking select.
	var got []string
	for {
		select {
		case e := <-h.Events():
			got = append(got, e.Content)
		default:
			assert.Equal(t, []string{"a", "b"}, got)
			return
		}
	}
}

// TestHub_SendConcurrent exercises concurrent Send from many goroutines to
// surface data races under -race. Because Send uses a non-blocking send with a
// drop-on-full policy, this test asserts only that: (1) every received event
// was sent (received <= sent), (2) the vast majority of events make it through
// (no significant loss under a large buffer), and (3) the run completes without
// panicking. The buffer is sized generously to avoid drops under normal timing.
func TestHub_SendConcurrent(t *testing.T) {
	h := NewHub(4096)
	defer h.Close()

	const producers = 16
	const perProducer = 100
	var sent int64
	ready := make(chan struct{})
	for i := 0; i < producers; i++ {
		go func() {
			<-ready
			for j := 0; j < perProducer; j++ {
				if h.Send(TokenEvent(AgentConfuse, "x")) {
					atomic.AddInt64(&sent, 1)
				}
			}
		}()
	}
	close(ready)

	// Drain until Done or timeout. We give producers time to finish by waiting
	// on a quiet channel rather than a fixed target.
	var received int64
	quiet := make(chan struct{})
	go func() {
		var last int64
		for {
			time.Sleep(20 * time.Millisecond)
			cur := atomic.LoadInt64(&received)
			if cur == last && cur > 0 {
				close(quiet)
				return
			}
			last = cur
		}
	}()

drain:
	for {
		select {
		case e := <-h.Events():
			_ = e
			atomic.AddInt64(&received, 1)
		case <-quiet:
			break drain
		case <-time.After(3 * time.Second):
			t.Fatalf("drain timed out: received=%d sent=%d", atomic.LoadInt64(&received), atomic.LoadInt64(&sent))
		}
	}

	sentFinal := atomic.LoadInt64(&sent)
	receivedFinal := atomic.LoadInt64(&received)
	assert.Greater(t, sentFinal, int64(0), "no sends succeeded")
	assert.LessOrEqual(t, receivedFinal, sentFinal, "received more than sent")
	// With a 4096-buffer and bursty sends we expect negligible loss. Allow a
	// small tolerance for scheduler timing.
	assert.Greater(t, receivedFinal, sentFinal*9/10,
		"too many events dropped: received=%d sent=%d", receivedFinal, sentFinal)
}
