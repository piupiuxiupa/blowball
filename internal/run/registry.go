package run

import (
	"context"
	"sync"
)

// Registry is the process-local table of running turns keyed by run id. It
// owns nothing durable — Redis holds the cross-process truth — it only maps a
// run id to its cancel function and completion signal so the cancel endpoint
// (and graceful shutdown) can reach a turn running in THIS process.
//
// Entries are registered when the turn starts and unregistered when the turn
// goroutine returns. A cancel arriving after unregister finds no entry and
// falls back to the Redis flag / dead-run paths.
type Registry struct {
	mu   sync.Mutex
	runs map[string]*Run
}

// Run is one registered running turn.
type Run struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	finish sync.Once
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{runs: make(map[string]*Run)}
}

// Register adds a run under id with its turn-context cancel function. The
// caller must Unregister when the turn goroutine returns.
func (r *Registry) Register(id string, cancel context.CancelFunc) *Run {
	rr := &Run{id: id, cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	r.runs[id] = rr
	r.mu.Unlock()
	return rr
}

// Get returns the run registered under id.
func (r *Registry) Get(id string) (*Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rr, ok := r.runs[id]
	return rr, ok
}

// Unregister removes the run from the table. The Run's Done channel stays
// readable for anyone already waiting on it.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	delete(r.runs, id)
	r.mu.Unlock()
}

// Cancel cancels the run's turn context if it is registered in this process.
// Returns false when the id is unknown here (other replica, or already
// finished) — the caller then falls back to the Redis cancel flag.
func (r *Registry) Cancel(id string) bool {
	rr, ok := r.Get(id)
	if !ok {
		return false
	}
	rr.cancel()
	return true
}

// CancelAll cancels every running turn registered in this process and returns
// the number cancelled. Used by graceful shutdown so in-flight turns wind
// down (persisting partial output) within the bounded shutdown window.
func (r *Registry) CancelAll() int {
	r.mu.Lock()
	runs := make([]*Run, 0, len(r.runs))
	for _, rr := range r.runs {
		runs = append(runs, rr)
	}
	r.mu.Unlock()
	for _, rr := range runs {
		rr.cancel()
	}
	return len(runs)
}

// WaitAll blocks until no runs remain registered or ctx is done. Runs
// unregister themselves when their goroutine returns, so "empty" means every
// turn observed its cancellation and finished. New registrations during the
// wait are also awaited (loop re-snapshots while non-empty).
func (r *Registry) WaitAll(ctx context.Context) error {
	for {
		r.mu.Lock()
		runs := make([]*Run, 0, len(r.runs))
		for _, rr := range r.runs {
			runs = append(runs, rr)
		}
		r.mu.Unlock()
		if len(runs) == 0 {
			return nil
		}
		done := make([]<-chan struct{}, 0, len(runs))
		for _, rr := range runs {
			done = append(done, rr.done)
		}
		for _, d := range done {
			select {
			case <-d:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Finish marks the run complete (idempotent). The turn goroutine calls it
// right before unregistering; WaitAll observers unblock on it.
func (rr *Run) Finish() {
	rr.finish.Do(func() { close(rr.done) })
}

// Done returns the completion signal closed by Finish.
func (rr *Run) Done() <-chan struct{} { return rr.done }

// ID returns the run id.
func (rr *Run) ID() string { return rr.id }
