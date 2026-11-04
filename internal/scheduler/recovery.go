package scheduler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	runpkg "github.com/lush/blowball/internal/run"
)

func (s *Scheduler) recover(ctx context.Context) error {
	if s.runs == nil {
		return nil
	}
	runs, err := s.store.ListScheduledRunsForRecovery(ctx, s.cfg.ScanBatch)
	if err != nil {
		return err
	}
	for i := range runs {
		occurrence := &runs[i]
		if s.isActive(occurrence.OccurrenceID) {
			continue
		}
		meta, found, err := s.runs.Store.GetMeta(ctx, occurrence.RunID)
		if err != nil {
			logger.L().Warn("scheduled recovery run-meta read failed", zap.String("op", "scheduler.recover"), zap.String("run_id", occurrence.RunID), zap.Error(err))
			continue
		}
		if found && runpkg.Terminal(meta.Status) {
			s.finishTerminalWithRetry(occurrence.OccurrenceID, scheduledTerminalStatus(meta.Status), "", nil)
			continue
		}
		alive, err := s.runs.Store.Alive(ctx, occurrence.RunID)
		if err != nil {
			logger.L().Warn("scheduled recovery heartbeat read failed", zap.String("op", "scheduler.recover"), zap.String("run_id", occurrence.RunID), zap.Error(err))
			continue
		}
		if alive || s.recoveryGrace(occurrence) {
			if !s.trackRecovered(occurrence) {
				return nil
			}
			continue
		}
		s.finishTerminalWithRetry(occurrence.OccurrenceID, model.ScheduledRunInterrupted, model.ScheduledReasonResultUnknown, nil)
	}
	return nil
}

func (s *Scheduler) recoveryGrace(run *model.ScheduledTaskRun) bool {
	reference := run.UpdateTime
	if run.StartedAt != nil {
		reference = *run.StartedAt
	}
	return reference.IsZero() || s.now().UTC().Sub(reference) < 2*runpkg.HeartbeatTTL
}

func (s *Scheduler) isActive(occurrenceID string) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	_, ok := s.active[occurrenceID]
	return ok
}

func (s *Scheduler) trackRecovered(run *model.ScheduledTaskRun) bool {
	ctx, cancel := context.WithCancel(s.ctx)
	if !s.beginWorker(run.OccurrenceID, cancel) {
		cancel()
		return false
	}
	go func() {
		defer s.endWorker(run.OccurrenceID)
		s.watchRecovered(ctx, run)
	}()
	return true
}

// watchRecovered tracks another process's live run without cancelling it on
// local shutdown and without ever invoking the Agent again.
func (s *Scheduler) watchRecovered(ctx context.Context, occurrence *model.ScheduledTaskRun) {
	interval := s.cfg.PollInterval
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			meta, found, err := s.runs.Store.GetMeta(ctx, occurrence.RunID)
			if err != nil {
				continue
			}
			if found && runpkg.Terminal(meta.Status) {
				s.finishTerminalWithRetry(occurrence.OccurrenceID, scheduledTerminalStatus(meta.Status), "", nil)
				return
			}
			alive, err := s.runs.Store.Alive(ctx, occurrence.RunID)
			if err != nil {
				continue
			}
			if !alive && !s.recoveryGrace(occurrence) {
				s.finishTerminalWithRetry(occurrence.OccurrenceID, model.ScheduledRunInterrupted, model.ScheduledReasonResultUnknown, nil)
				return
			}
		}
	}
}
