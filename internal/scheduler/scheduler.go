package scheduler

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
)

// Start launches one scheduler instance's recovery and polling loop.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.loop()
	})
}

func (s *Scheduler) loop() {
	defer s.wg.Done()
	s.logCycle(s.RunOnce(s.ctx))
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.logCycle(s.RunOnce(s.ctx))
		}
	}
}

func (s *Scheduler) logCycle(err error) {
	if err != nil {
		logger.L().Warn("scheduler cycle completed with errors", zap.String("op", "scheduler.cycle"), zap.Error(err))
	}
}

// RunOnce performs bounded recovery, materialization, and dispatch. Workers
// continue asynchronously; call Shutdown or waitIdle to drain them.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	if s.ctx.Err() != nil {
		return nil
	}
	var errs []error
	if err := s.recover(ctx); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.store.MaterializeScheduledTasks(ctx, s.now().UTC(), s.cfg.ScanBatch); err != nil {
		errs = append(errs, err)
	}
	runs, err := s.store.ListClaimableScheduledRuns(ctx, s.now().UTC(), s.cfg.ScanBatch)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	for i := range runs {
		if !s.dispatch(&runs[i]) {
			break // capacity or shutdown: leave the remainder durable/pending
		}
	}
	return errors.Join(errs...)
}

func (s *Scheduler) dispatch(run *model.ScheduledTaskRun) bool {
	ctx, cancel := context.WithCancel(s.ctx)
	if !s.beginWorker(run.OccurrenceID, cancel) {
		cancel()
		return false
	}
	go func() {
		defer s.endWorker(run.OccurrenceID)
		s.execute(ctx, run)
	}()
	return true
}

func (s *Scheduler) beginWorker(occurrenceID string, cancel context.CancelFunc) bool {
	if s.ctx.Err() != nil {
		return false
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if _, exists := s.active[occurrenceID]; exists {
		<-s.sem
		return false
	}
	s.active[occurrenceID] = cancel
	s.wg.Add(1)
	return true
}

func (s *Scheduler) endWorker(occurrenceID string) {
	s.activeMu.Lock()
	delete(s.active, occurrenceID)
	s.activeMu.Unlock()
	<-s.sem
	s.wg.Done()
}

// Shutdown stops new dispatch, cancels preparations and scheduler-owned runs,
// then waits for bounded terminal persistence. The shared run registry's
// graceful-shutdown drain covers the same started turns at process level.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(s.cancel)
	s.activeMu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.activeMu.Unlock()
	return s.waitIdle(ctx)
}

func (s *Scheduler) waitIdle(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
