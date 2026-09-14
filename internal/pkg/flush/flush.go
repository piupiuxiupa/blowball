// Package flush provides the shared pacing loop behind the write-behind
// flushers (internal/msgflush, internal/llmraw): one goroutine that wakes on
// an interval ticker, an optional producer nudge (which triggers an early
// flush once the queue crosses the batch threshold), or stop, plus a final
// bounded drain on Close. The Flush callback owns the claim/insert/ack
// semantics of each pipeline; this package owns only when to run it.
package flush

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
)

// errorBackoff parks the loop briefly after a Redis/MySQL failure so a dead
// dependency is retried at a calm pace instead of hot-looping.
const errorBackoff = time.Second

// Params configures a Pump.
type Params struct {
	// Name prefixes log lines ("msgflush", "llmraw").
	Name string
	// Interval is the ticker bounding how long a queued record can sit in the
	// buffer before it is flushed.
	Interval time.Duration
	// BatchSize is the queue-length threshold the nudge path checks: a nudge
	// below it does not flush (the ticker owns those).
	BatchSize int
	// DrainTimeout bounds the final drain Close performs after the loop exits.
	DrainTimeout time.Duration
	// QueueLen reports the current buffer length; consulted on nudges and to
	// annotate failure logs.
	QueueLen func(ctx context.Context) (int64, error)
	// Flush drains what it can, bounded by ctx. An error backs the loop off
	// briefly and retries on the next wake. It deliberately never consults
	// stop: Close's final drain is this same call with a DrainTimeout-bounded
	// ctx.
	Flush func(ctx context.Context) error
	// OnStart (optional) runs once in Start before the loop launches, e.g.
	// crash-recovery requeueing. A failure it reports must not prevent the
	// loop from starting.
	OnStart func()
	// Notify (optional) is the producer wakeup channel; a signal flushes
	// early once the queue crosses BatchSize. Nil disables the nudge (a
	// select on a nil channel never fires).
	Notify chan struct{}
}

// Pump is the pacing loop behind a write-behind flusher. Exactly one
// goroutine runs the loop (Start); Close stops it and runs one final bounded
// drain.
type Pump struct {
	p Params

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	// started guards Close against waiting on done for a loop that never
	// launched (Close-without-Start must be a bounded no-op, not a hang).
	started atomic.Bool
}

// New builds a Pump from p.
func New(p Params) *Pump {
	return &Pump{p: p, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs OnStart (if any) and launches the loop goroutine. Callers MUST
// call Close when done. Starting twice is a no-op.
func (p *Pump) Start() {
	if p.started.CompareAndSwap(false, true) {
		if p.p.OnStart != nil {
			p.p.OnStart()
		}
		go p.loop()
	}
}

// loop wakes on ticker, producer nudge, or stop; on wake it flushes. Errors
// back off briefly rather than tearing the loop down.
func (p *Pump) loop() {
	defer close(p.done)

	ticker := time.NewTicker(p.p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.runRound()
		case <-p.p.Notify:
			// The nudge only triggers an immediate flush when the queue has
			// crossed the batch threshold; below it the ticker path owns
			// flushing (not every push means not every push flushes).
			n, err := p.p.QueueLen(context.Background())
			if err != nil {
				logger.L().Warn(p.p.Name+" queue length check failed", zap.Error(err))
				time.Sleep(errorBackoff)
				continue
			}
			if n < int64(p.p.BatchSize) {
				continue
			}
			p.runRound()
		}
	}
}

// runRound flushes once; on failure it logs (with the queue length) and backs
// off so a dead dependency is retried calmly instead of hot-looping.
func (p *Pump) runRound() {
	if err := p.p.Flush(context.Background()); err != nil {
		fields := []zap.Field{zap.Error(err)}
		if n, lerr := p.p.QueueLen(context.Background()); lerr == nil {
			fields = append(fields, zap.Int64("queue_len", n))
		}
		logger.L().Warn(p.p.Name+" flush failed; backing off and retrying", fields...)
		time.Sleep(errorBackoff)
	}
}

// Close stops the loop and performs one final bounded drain so a graceful
// shutdown does not leave the tail of the buffer unflushed. It is idempotent
// and safe on a Pump that was never Start-ed (a bounded no-op drain).
func (p *Pump) Close() {
	p.stopOnce.Do(func() {
		close(p.stop)
	})
	// Wait for the loop to exit before draining, so the final flush is the
	// only thing touching the buffer.
	if p.started.Load() {
		<-p.done
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.p.DrainTimeout)
	defer cancel()
	if err := p.p.Flush(ctx); err != nil {
		logger.L().Warn(p.p.Name+" final drain failed", zap.Error(err))
	}
}
