package msgflush

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// Flusher pacing constants. errorBackoff and finalDrainTimeout are
// deliberately not configurable (mirroring internal/llmraw); the interval and
// batch size are config-driven via Config.
const (
	// errorBackoff parks the loop briefly after a Redis/MySQL failure so a
	// dead dependency is retried at a calm pace instead of hot-looping.
	errorBackoff = 1 * time.Second
	// finalDrainTimeout bounds the drain attempt Close makes on shutdown.
	finalDrainTimeout = 5 * time.Second
	// drainBatchSize caps the claim chunk used by the package-level Drain
	// primitive, which has no operator-configured batch size.
	drainBatchSize = 100
)

// claimedMsg is one record atomically claimed off the ingest queue: the exact
// canonical JSON blob (raw, the LREM-ack key) and its decoded form.
type claimedMsg struct {
	raw []byte
	msg model.Message
}

// Flusher moves queued messages from the Redis ingest queue into MySQL in
// batches. Exactly one goroutine per process runs the loop (Start); in a
// multi-agent-process deployment every process runs its own flusher against
// the shared queue — safe because each claim is an atomic LMOVE, so a record
// lands in exactly one flusher's batch.
//
// Claim/ack protocol (reliable-queue semantics): LMOVE buffer→processing
// claims, INSERT IGNORE persists, LREM acks. A record that has been claimed
// but not yet acked when a round fails stays parked in processing and is
// retried by that flusher (the in-memory pending batch) or, after a crash, by
// the next process start (startup recovery requeues processing→buffer).
type Flusher struct {
	buf   Buffer
	store MessageStore

	interval  time.Duration
	batchSize int

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	// started guards Close against waiting on done for a loop that never
	// launched (Close-without-Start must be a bounded no-op, not a hang).
	started atomic.Bool

	// notify is the producer-side wakeup channel (shared with the
	// SessionService write path); when non-nil a signal selects the
	// flush-now path once the queue has crossed batchSize. A nil channel
	// disables the nudge (select on nil never fires).
	notify chan struct{}

	// pending is the stuck batch: claimed (still parked in msgs:processing)
	// but not yet fully inserted and acked. It is retried head-of-line at the
	// next round until it resolves. Only the loop goroutine — and Close's
	// final drain, which runs strictly after the loop has exited — touches
	// it, so no lock is needed.
	pending []claimedMsg
}

// NewFlusher builds a Flusher over buf and store. cfg zero values fall back
// to the package defaults. notify (optional) is the wakeup channel producers
// signal after each dual write; the loop uses it to flush early once the
// queue crosses the batch threshold. Callers MUST call Start, then Close.
func NewFlusher(buf Buffer, store MessageStore, cfg Config, notify chan struct{}) *Flusher {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultFlushBatchSize
	}
	return &Flusher{
		buf:       buf,
		store:     store,
		interval:  interval,
		batchSize: batchSize,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		notify:    notify,
	}
}

// Start performs the startup crash recovery (requeue any msgs:processing
// residue from a previous process back onto the buffer head) and launches the
// flush loop goroutine. Callers MUST call Close when done. Starting twice is
// a no-op. A recovery failure is logged but not fatal: the loop still runs,
// and the residue is requeued by the next process start.
func (f *Flusher) Start() {
	if f.started.CompareAndSwap(false, true) {
		if err := f.buf.RecoverProcessingToBuffer(context.Background()); err != nil {
			logger.L().Error("msgflush startup recovery failed; processing residue stays parked until the next restart",
				zap.Error(err))
		} else {
			logger.L().Info("msgflush flusher started",
				zap.Duration("interval", f.interval),
				zap.Int("batch_size", f.batchSize))
		}
		go f.loop()
	}
}

// loop is the main flush cycle: wake on ticker, producer nudge, or stop; on
// wake, flush. Errors back off briefly rather than tearing the loop down —
// messages must never be dropped, so the queue simply grows and the loop
// keeps retrying (with WARN logs carrying the queue length for the operator).
func (f *Flusher) loop() {
	defer close(f.done)

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
			f.runRound()
		case <-f.notify:
			// The nudge only triggers an immediate flush when the queue has
			// crossed the batch threshold; below it the ticker path owns
			// flushing (not every write means not every push flushes).
			n, err := f.buf.LenMessageBuffer(context.Background())
			if err != nil {
				logger.L().Warn("msgflush length check failed", zap.Error(err))
				time.Sleep(errorBackoff)
				continue
			}
			if n < int64(f.batchSize) {
				continue
			}
			f.runRound()
		}
	}
}

// runRound runs one flushAll and logs (with queue length) + backs off on
// failure.
func (f *Flusher) runRound() {
	if err := f.flushAll(context.Background()); err != nil {
		fields := []zap.Field{zap.Error(err)}
		if n, lerr := f.buf.LenMessageBuffer(context.Background()); lerr == nil {
			fields = append(fields, zap.Int64("queue_len", n))
		}
		logger.L().Warn("msgflush flush failed; backing off and retrying", fields...)
		time.Sleep(errorBackoff)
	}
}

// flushAll drains the queue in batch-size chunks until empty (or ctx ends):
// the stuck pending batch is retried first, then fresh claims are processed
// round by round. Each chunk is claimed atomically, inserted idempotently,
// and acknowledged per record. The returned error signals a dependency-level
// failure (Redis/MySQL unreachable) so the caller can back off; the parked
// records are never dropped and are retried on the next round.
//
// It deliberately does NOT consult f.stop — Close closes stop first and then
// relies on this drain, so a stop-check here would make the final flush a
// no-op (boundedness comes from the ctx Close passes in).
func (f *Flusher) flushAll(ctx context.Context) error {
	// 1) Head-of-line retry of the stuck batch. Its records are parked in
	// msgs:processing; retrying before any fresh claim preserves order and
	// keeps the failure isolated to this batch.
	if len(f.pending) > 0 {
		remaining, err := f.processBatch(ctx, f.pending)
		f.pending = remaining
		if err != nil || len(remaining) > 0 {
			return err
		}
	}

	// 2) Claim and process fresh rounds until the queue drains.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		claimed, cerr := f.claim(ctx, f.batchSize)
		// Records claimed before a mid-claim failure are already parked in
		// processing — process them before surfacing the error.
		if len(claimed) > 0 {
			remaining, perr := f.processBatch(ctx, claimed)
			if perr != nil || len(remaining) > 0 {
				f.pending = remaining
				return perr
			}
		}
		if cerr != nil {
			return cerr
		}
		if len(claimed) == 0 {
			return nil
		}
	}
}

// claim atomically moves up to max records off the ingest queue (one LMOVE
// per record) and decodes them. A record whose JSON cannot decode into a
// model.Message can never be inserted — it is dead-lettered immediately
// (acked + ERROR log with the raw payload size) rather than poisoning the
// queue; decode failures are a producer bug, not a transient condition.
func (f *Flusher) claim(ctx context.Context, max int) ([]claimedMsg, error) {
	out := make([]claimedMsg, 0, max)
	for len(out) < max {
		raw, err := f.buf.MoveMessageToProcessing(ctx)
		if err != nil {
			return out, err
		}
		if raw == nil {
			return out, nil
		}
		var m model.Message
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			logger.L().Error("msgflush dead-letter: queued record is not valid message JSON",
				zap.Int("raw_bytes", len(raw)),
				zap.Error(jerr))
			// The ack is best-effort: on failure the record stays parked in
			// processing and dead-letters again after the next startup
			// recovery or drain. It must never be tracked as a claimable
			// message — that would insert an empty row.
			if aerr := f.buf.RemoveFromProcessing(ctx, raw); aerr != nil {
				logger.L().Warn("msgflush dead-letter ack failed; record stays parked in processing",
					zap.Error(aerr))
			}
			continue
		}
		out = append(out, claimedMsg{raw: raw, msg: m})
	}
	return out, nil
}

// processBatch inserts the claimed batch and resolves every record: each is
// either acked (LREM from msgs:processing) or dead-lettered. The returned
// slice holds records that remain parked in msgs:processing (unresolved);
// the returned error is non-nil when a dependency failed, signalling the
// caller to back off. Records are dropped ONLY via the two dead-letter
// cases: a deleted session (MySQL FK 1452) and an undecodable record.
func (f *Flusher) processBatch(ctx context.Context, batch []claimedMsg) ([]claimedMsg, error) {
	msgs := make([]model.Message, 0, len(batch))
	for _, c := range batch {
		msgs = append(msgs, c.msg)
	}

	_, err := f.store.AppendMessages(ctx, msgs)
	if err == nil {
		unresolved, aerr := f.ackAll(ctx, batch)
		f.refreshSessionTimes(ctx, batch)
		if aerr != nil || len(unresolved) > 0 {
			return unresolved, aerr
		}
		return nil, nil
	}
	if !isFKError(err) {
		// Dependency or unknown failure: the whole batch stays parked and is
		// retried after the backoff — never dropped.
		return batch, err
	}

	// FK failure — at least one record references a deleted session and can
	// never insert. Isolate per record so the retryable majority is not
	// poisoned by the dead rows: insert each record alone; FK failures are
	// dead-lettered (acked + ERROR with a record summary), anything else
	// parks for retry.
	logger.L().Warn("msgflush batch insert hit foreign-key error; isolating per record",
		zap.Int("rows", len(batch)),
		zap.Error(err))

	var unresolved []claimedMsg
	var resolved []claimedMsg
	for _, c := range batch {
		_, ierr := f.store.AppendMessages(ctx, []model.Message{c.msg})
		switch {
		case ierr == nil:
			if rerr := f.buf.RemoveFromProcessing(ctx, c.raw); rerr != nil {
				// Inserted but not acked: park for a re-ack; the idempotent
				// insert absorbs the duplicate.
				unresolved = append(unresolved, c)
				continue
			}
			resolved = append(resolved, c)
		case isFKError(ierr):
			logger.L().Error("msgflush dead-letter: message references a deleted session",
				zap.String("session_id", c.msg.SessionID),
				zap.String("client_msg_id", c.msg.ClientMsgID),
				zap.String("agent", c.msg.Agent),
				zap.String("event_type", c.msg.EventType),
				zap.Int("msg_index", c.msg.MsgIndex),
				zap.Int("content_bytes", len(c.msg.Content)),
				zap.String("trace_id", c.msg.TraceID))
			if rerr := f.buf.RemoveFromProcessing(ctx, c.raw); rerr != nil {
				unresolved = append(unresolved, c)
			}
		default:
			unresolved = append(unresolved, c)
		}
	}
	if len(resolved) > 0 {
		f.refreshSessionTimes(ctx, resolved)
	}
	if len(unresolved) > 0 {
		return unresolved, fmt.Errorf("msgflush: %d record(s) unresolved after FK isolation", len(unresolved))
	}
	return nil, nil
}

// ackAll acknowledges every record in the batch (LREM by the exact claimed
// blob). Records whose ack fails are returned as unresolved so the caller can
// park them; the inserts already committed, and the idempotent insert absorbs
// the duplicate a redelivery would create.
func (f *Flusher) ackAll(ctx context.Context, batch []claimedMsg) ([]claimedMsg, error) {
	var unresolved []claimedMsg
	for _, c := range batch {
		if err := f.buf.RemoveFromProcessing(ctx, c.raw); err != nil {
			unresolved = append(unresolved, c)
		}
	}
	if len(unresolved) > 0 {
		return unresolved, fmt.Errorf("msgflush: ack failed for %d record(s)", len(unresolved))
	}
	return nil, nil
}

// refreshSessionTimes touches sessions.update_time once per distinct session
// in the supplied records, so recently-active sessions keep bubbling to the
// top of the session list (the synchronous per-turn refresh moved here with
// the write-behind change). Best-effort: a failure is logged and never
// affects the already-committed inserts or later batches.
func (f *Flusher) refreshSessionTimes(ctx context.Context, batch []claimedMsg) {
	seen := make(map[string]struct{}, len(batch))
	for _, c := range batch {
		if _, ok := seen[c.msg.SessionID]; ok {
			continue
		}
		seen[c.msg.SessionID] = struct{}{}
		if err := f.store.UpdateSessionTime(ctx, c.msg.SessionID); err != nil {
			logger.L().Warn("msgflush update session time failed",
				zap.String("session_id", c.msg.SessionID),
				zap.Error(err))
		}
	}
}

// Close stops the loop and performs one final bounded drain so a graceful
// shutdown does not leave the tail of the queue unflushed. It is idempotent
// and safe on a flusher that was never Start-ed (a bounded no-op drain).
func (f *Flusher) Close() {
	f.stopOnce.Do(func() {
		close(f.stop)
	})
	// Wait for the loop to exit before draining, so the final flush is the
	// only thing touching the queue.
	if f.started.Load() {
		<-f.done
	}

	ctx, cancel := context.WithTimeout(context.Background(), finalDrainTimeout)
	defer cancel()
	if err := f.flushAll(ctx); err != nil {
		logger.L().Warn("msgflush final drain failed; tail stays queued for the next process",
			zap.Error(err))
	}
}

// Drain is the synchronous drain primitive shared by the session-delete flow
// and the read-miss recovery path: it requeues any processing residue (an
// in-flight batch parked by a failed drain or a crashed process), then
// claims, inserts and acks records until both lists are empty. It is bounded
// by ctx — the caller sets the deadline. FK failures are dead-lettered
// exactly like the background flusher does; any other failure returns with
// the unprocessed records safely parked in msgs:processing, where a later
// drain or the next process start picks them up.
//
// Drain does not need a running Flusher: it borrows the claim/insert/ack
// machinery on the calling goroutine, so the api role (which never
// constructs a flusher) can still drain before deleting a session.
func Drain(ctx context.Context, buf Buffer, store MessageStore) error {
	if err := buf.RecoverProcessingToBuffer(ctx); err != nil {
		return fmt.Errorf("msgflush drain: recover processing: %w", err)
	}
	f := &Flusher{buf: buf, store: store, batchSize: drainBatchSize}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("msgflush drain: %w", ctx.Err())
		default:
		}

		claimed, err := f.claim(ctx, drainBatchSize)
		if len(claimed) > 0 {
			if _, perr := f.processBatch(ctx, claimed); perr != nil {
				return fmt.Errorf("msgflush drain: insert: %w", perr)
			}
		}
		if err != nil {
			return fmt.Errorf("msgflush drain: claim: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
	}
}
