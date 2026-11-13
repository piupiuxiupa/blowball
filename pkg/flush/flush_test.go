package flush

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func pumpParams(flush func(context.Context) error, queueLen func(context.Context) (int64, error), notify chan struct{}) Params {
	return Params{
		Name:         "test",
		Interval:     time.Hour, // ticker effectively disabled; tests drive via nudge/Close
		BatchSize:    3,
		DrainTimeout: 2 * time.Second,
		QueueLen:     queueLen,
		Flush:        flush,
		Notify:       notify,
	}
}

func TestPump_NudgeFlushesOnlyAboveThreshold(t *testing.T) {
	var flushes atomic.Int64
	var qlen atomic.Int64
	notify := make(chan struct{}, 1)
	p := New(pumpParams(func(context.Context) error {
		flushes.Add(1)
		return nil
	}, func(context.Context) (int64, error) {
		return qlen.Load(), nil
	}, notify))
	p.Start()
	defer p.Close()

	notify <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	if got := flushes.Load(); got != 0 {
		t.Fatalf("below threshold: flushes = %d, want 0", got)
	}

	qlen.Store(3)
	notify <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for flushes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := flushes.Load(); got != 1 {
		t.Fatalf("at threshold: flushes = %d, want 1", got)
	}
}

func TestPump_CloseRunsFinalDrain(t *testing.T) {
	var flushes atomic.Int64
	p := New(pumpParams(func(context.Context) error {
		flushes.Add(1)
		return nil
	}, func(context.Context) (int64, error) { return 0, nil }, nil))
	p.Start()
	p.Close()
	if got := flushes.Load(); got != 1 {
		t.Fatalf("flushes = %d, want 1 (final drain)", got)
	}
	// Close again: the stop is idempotent and bounded; the drain reruns but
	// the queue is already empty, so it is a cheap no-op in production.
	p.Close()
	if got := flushes.Load(); got != 2 {
		t.Fatalf("after second Close: flushes = %d, want 2 (repeat drain)", got)
	}
}

func TestPump_CloseWithoutStartBoundedNoop(t *testing.T) {
	p := New(pumpParams(func(context.Context) error { return nil },
		func(context.Context) (int64, error) { return 0, nil }, nil))
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close without Start hung")
	}
}
