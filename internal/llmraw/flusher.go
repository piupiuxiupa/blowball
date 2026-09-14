package llmraw

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/flush"
	"github.com/lush/blowball/internal/pkg/logger"
)

// Flusher pacing constants (deliberately not configurable — the
// llm-raw-capture spec's zero-config always-on decision).
const (
	// flushInterval is the ticker that bounds how long a captured record can
	// sit in the buffer before it lands in MySQL.
	flushInterval = 5 * time.Second
	// flushBatchSize is the queue-length trigger (and the pop chunk size):
	// once the buffer holds this many records the sink's nudge flushes early.
	flushBatchSize = 50
	// finalFlushTimeout bounds the drain attempt Close makes on shutdown.
	finalFlushTimeout = 3 * time.Second
)

// Flusher moves captured raw records from the Redis buffer into MySQL in
// batches. Exactly one goroutine per process runs the loop (Start); in a
// multi-agent-process deployment every process runs its own flusher against
// the shared buffer — safe because the pop is an atomic LPOP count, so each
// record lands in exactly one process's batch.
type Flusher struct {
	buf   Buffer
	store RawLogStore

	// pump owns the pacing loop (ticker + sink nudge + final drain);
	// flushAll is its Flush callback.
	pump *flush.Pump
}

// NewFlusher builds a Flusher over buf and store. notify (optional, shared
// with the Sink) wakes the loop between ticks so a burst can trigger an early
// flush once the queue has crossed flushBatchSize.
func NewFlusher(buf Buffer, store RawLogStore, notify chan struct{}) *Flusher {
	f := &Flusher{buf: buf, store: store}
	f.pump = flush.New(flush.Params{
		Name:         "llmraw",
		Interval:     flushInterval,
		BatchSize:    flushBatchSize,
		DrainTimeout: finalFlushTimeout,
		QueueLen:     buf.LenRawLogs,
		Flush:        f.flushAll,
		Notify:       notify,
	})
	return f
}

// Start launches the flush loop goroutine. Callers MUST call Close when done.
// Starting twice is a no-op.
func (f *Flusher) Start() { f.pump.Start() }

// flushAll drains the buffer in batch-size chunks until empty (or ctx ends).
// Each chunk is popped atomically, decoded, and inserted; a decode failure
// drops the single malformed record (logged) rather than the chunk. The
// returned error signals a dependency-level failure (Redis/MySQL unreachable)
// so the caller can back off; per-record problems are already handled inside.
// It deliberately does NOT consult f.stop — Close closes stop first and then
// relies on this drain, so a stop-check here would make the final flush a
// no-op (boundedness comes from the ctx Close passes in).
func (f *Flusher) flushAll(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		raws, err := f.buf.PopRawLogs(ctx, flushBatchSize)
		if err != nil {
			return err
		}
		if len(raws) == 0 {
			return nil
		}

		logs := make([]model.LLMRawLog, 0, len(raws))
		for _, raw := range raws {
			var l model.LLMRawLog
			if err := json.Unmarshal(raw, &l); err != nil {
				logger.L().Warn("llmraw flusher decode failed; dropping record",
					zap.Int("raw_bytes", len(raw)),
					zap.Error(err))
				continue
			}
			logs = append(logs, l)
		}
		if len(logs) == 0 {
			continue
		}

		if err := f.store.AppendRawLogs(ctx, logs); err != nil {
			// The popped chunk is lost (at-most-once); surface the failure so
			// the loop backs off and the next cycle retries with fresh data.
			logger.L().Error("llmraw flusher insert failed; dropping batch",
				zap.Int("rows", len(logs)),
				zap.Error(err))
			return err
		}
		logger.L().Debug("llmraw flushed", zap.Int("rows", len(logs)))
	}
}

// Close stops the loop and performs one final bounded drain so a graceful
// shutdown does not leave the tail of the buffer unflushed. It is idempotent
// and safe on a flusher that was never Start-ed (a bounded no-op drain).
func (f *Flusher) Close() { f.pump.Close() }
