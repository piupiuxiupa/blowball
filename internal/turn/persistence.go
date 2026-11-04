package turn

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/run"
)

const (
	turnPersistBudget       = 30 * time.Second
	turnPersistAttempts     = 3
	turnPersistRetryBackoff = time.Second
)

var incrementalPersistEvery = time.Second

// SetIncrementalPersistEvery adjusts the polling cadence and returns the old
// value. It exists for focused tests; production wiring never calls it.
func SetIncrementalPersistEvery(every time.Duration) time.Duration {
	old := incrementalPersistEvery
	incrementalPersistEvery = every
	return old
}

func startPersistHeartbeat(saveCtx context.Context, store run.Store, runID, sessionID string, every time.Duration) (stop func()) {
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ticker.C:
				hbCtx, hbCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := store.Heartbeat(hbCtx, runID, sessionID); err != nil {
					logger.FromContext(saveCtx).Warn("persist-phase heartbeat failed",
						zap.String("op", "turn.persist"),
						zap.String("run_id", runID),
						zap.Error(err))
				}
				hbCancel()
			}
		}
	}()
	return func() {
		once.Do(func() { close(hbStop) })
		<-hbDone
	}
}

func (e *Executor) startIncrementalPersister(
	saveCtx context.Context,
	tap *TurnEventTap,
	persist PersistInfo,
	flushed *FlushState,
) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(incrementalPersistEvery)
		defer ticker.Stop()
		log := logger.FromContext(saveCtx).With(
			zap.String("op", "turn.incremental_persist"),
			zap.String("run_id", persist.TraceID),
		)
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				snapCtx, snapCancel := context.WithTimeout(saveCtx, incrementalPersistEvery)
				events := tap.Snapshot(snapCtx)
				snapCancel()
				if events == nil {
					continue
				}
				merged := MergeEvents(events)
				flushed.PersistMu.Lock()
				batch, consumed := persist.BuildRoundFinalBatch(merged, flushed.Count(), flushed.UserPersisted(),
					time.Now().UTC(), RoundRenderOptions{})
				if len(batch.Msgs) == 0 && len(batch.Calls) == 0 {
					flushed.PersistMu.Unlock()
					continue
				}
				if err := e.sessSvc.SaveFinalMessages(saveCtx, persist.UserID, batch.Msgs, batch.Calls); err != nil {
					flushed.PersistMu.Unlock()
					log.Warn("round persist failed; will retry next tick",
						zap.Int("rows", len(batch.Msgs)), zap.Error(err))
					continue
				}
				flushed.Mark(consumed)
				flushed.PersistMu.Unlock()
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

// StartPersistHeartbeat exposes the keeper to transport compatibility tests.
func StartPersistHeartbeat(saveCtx context.Context, store run.Store, runID, sessionID string, every time.Duration) func() {
	return startPersistHeartbeat(saveCtx, store, runID, sessionID, every)
}
