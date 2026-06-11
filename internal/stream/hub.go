package stream

import (
	"context"
	"sync"

	"github.com/lush/blowball/internal/pkg/logger"
	"go.uber.org/zap"
)

// DefaultHubBufferSize is the buffered channel size used by NewHub when the
// caller does not pass an explicit size. 256 is large enough to absorb a burst
// of token events while keeping memory bounded.
const DefaultHubBufferSize = 256

// Hub is a thread-safe, single-consumer buffered event channel with explicit
// lifecycle. Producers call Send/SendCtx; the SSE consumer reads from Events().
// Close is idempotent and signals completion through Done().
//
// Design note: the events channel is intentionally NEVER closed. Closing it
// would race with in-flight producers (a SendCtx blocked on h.ch<-e the moment
// Close runs would panic with "send on closed channel"). Instead, producers and
// consumers select against Done() to observe shutdown. This is the standard
// safe pattern when there are multiple senders and the exact close timing is
// hard to synchronize.
type Hub struct {
	ch     chan StreamEvent
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

// NewHub creates a Hub with a buffered event channel of the given size. If
// bufferSize <= 0, DefaultHubBufferSize is used.
func NewHub(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = DefaultHubBufferSize
	}
	return &Hub{
		ch:   make(chan StreamEvent, bufferSize),
		done: make(chan struct{}),
	}
}

// Send attempts to enqueue an event without blocking on a full buffer: if the
// buffer is full the event is dropped and a warning is logged. Returns false if
// the hub is closed.
//
// For goroutines that hold a request context prefer SendCtx so a cancelled
// request cannot wedge a producer against a slow consumer.
func (h *Hub) Send(e StreamEvent) bool {
	if h.IsClosed() {
		return false
	}

	select {
	case h.ch <- e:
		return true
	case <-h.done:
		return false
	default:
		logger.L().Warn("stream hub buffer full, dropping event",
			zap.String("type", e.Type),
			zap.String("agent", e.Agent))
		return true
	}
}

// SendCtx enqueues an event, returning false if ctx is done or the hub is
// closed before the send completes. Unlike Send it blocks on a full buffer
// until either space frees up, the context is cancelled, or the hub closes.
func (h *Hub) SendCtx(ctx context.Context, e StreamEvent) bool {
	if h.IsClosed() {
		return false
	}

	select {
	case h.ch <- e:
		return true
	case <-ctx.Done():
		return false
	case <-h.done:
		return false
	}
}

// Events returns the read-only event channel for consumers. The channel is NOT
// closed on Close (see the Hub doc comment for rationale); consumers must also
// select against Done() to detect shutdown.
func (h *Hub) Events() <-chan StreamEvent {
	return h.ch
}

// Done returns a channel that is closed when Close is called. Producers and
// consumers select against it to detect shutdown without racing a channel close.
func (h *Hub) Done() <-chan struct{} {
	return h.done
}

// IsClosed reports whether Close has been called. A true return means future
// Send/SendCtx calls will fail fast, but an in-flight send may still complete
// against the buffer; consumers should still drain Events() until Done().
func (h *Hub) IsClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Close shuts the hub down. It is idempotent: subsequent calls are no-ops. It
// closes the Done() channel so any producer or consumer blocked in a select
// against it unblocks immediately. The events channel itself is NOT closed.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
}
