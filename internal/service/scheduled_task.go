package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/tokens"
)

var ErrScheduledTaskNotFound = errors.New("scheduled task not found")

// ScheduledTaskError is an owner-safe validation error returned as HTTP 400.
type ScheduledTaskError struct {
	Code    string
	Message string
}

func (e *ScheduledTaskError) Error() string { return e.Message }

func scheduledTaskValidation(code, message string) error {
	return &ScheduledTaskError{Code: code, Message: message}
}

// ScheduledTaskStore is the CRUD-only persistence surface. It deliberately
// contains no Agent, LLM, tool, or sandbox dependency.
type ScheduledTaskStore interface {
	CreateScheduledTask(ctx context.Context, task *model.ScheduledTask) error
	GetScheduledTask(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error)
	ListScheduledTasks(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error)
	UpdateScheduledTask(ctx context.Context, task *model.ScheduledTask) (bool, error)
	DeleteScheduledTask(ctx context.Context, userID, taskID string, now time.Time) (bool, error)
	ListScheduledTaskRuns(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error)
	GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error)
}

type CreateScheduledTaskInput struct {
	Name            string
	Prompt          string
	Schedule        schedule.Rule
	Session         model.ScheduledSessionPolicy
	Model           string
	ReasoningEffort string
	Enabled         *bool
}

type UpdateScheduledTaskInput struct {
	Name            *string
	Prompt          *string
	Schedule        *schedule.Rule
	Session         *model.ScheduledSessionPolicy
	Model           *string
	ReasoningEffort *string
	Enabled         *bool
}

// ScheduledTaskService validates and manages tasks without constructing any
// agent runtime. now is injectable for deterministic time-rule tests.
type ScheduledTaskService struct {
	store          ScheduledTaskStore
	selection      modelselect.Config
	maxInputTokens int
	now            func() time.Time
}

func NewScheduledTaskService(store ScheduledTaskStore, selection modelselect.Config, maxInputTokens int, clocks ...func() time.Time) *ScheduledTaskService {
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	return &ScheduledTaskService{store: store, selection: selection, maxInputTokens: maxInputTokens, now: now}
}

func (s *ScheduledTaskService) Create(ctx context.Context, userID string, input CreateScheduledTaskInput) (*model.ScheduledTask, error) {
	if err := s.validateText(input.Name, input.Prompt); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if err := schedule.Validate(input.Schedule, now, true); err != nil {
		return nil, scheduledTaskValidation("INVALID_SCHEDULE", err.Error())
	}
	next, err := schedule.Next(input.Schedule, now)
	if err != nil {
		return nil, scheduledTaskValidation("INVALID_SCHEDULE", err.Error())
	}
	if _, code, msg := modelselect.Resolve(s.selection, input.Model, input.ReasoningEffort); code != "" {
		return nil, scheduledTaskValidation(code, msg)
	}
	taskID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("scheduled task: mint id: %w", err)
	}
	task := &model.ScheduledTask{
		TaskID: taskID.String(), UserID: userID,
		Name: strings.TrimSpace(input.Name), Prompt: input.Prompt,
		Model: strings.TrimSpace(input.Model), ReasoningEffort: strings.TrimSpace(input.ReasoningEffort),
		Enabled: true, Revision: 1, NextRunAt: scheduledTaskTime(next),
	}
	if input.Enabled != nil {
		task.Enabled = *input.Enabled
	}
	if !task.Enabled {
		task.PausedReason = model.ScheduledReasonTaskPaused
	}
	if err := s.applySession(ctx, userID, task, input.Session); err != nil {
		return nil, err
	}
	task.SetSchedule(input.Schedule)
	task.ApplyDefaults()
	if err := s.store.CreateScheduledTask(ctx, task); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID, task.TaskID)
}

func (s *ScheduledTaskService) Get(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	task, err := s.store.GetScheduledTask(ctx, userID, taskID)
	if err != nil || task == nil {
		if err == nil {
			err = ErrScheduledTaskNotFound
		}
		return nil, err
	}
	return task, nil
}

func (s *ScheduledTaskService) List(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error) {
	limit, offset = scheduledTaskPage(limit, offset)
	return s.store.ListScheduledTasks(ctx, userID, limit, offset)
}

func (s *ScheduledTaskService) Runs(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	if _, err := s.Get(ctx, userID, taskID); err != nil {
		return nil, err
	}
	limit, offset = scheduledTaskPage(limit, offset)
	return s.store.ListScheduledTaskRuns(ctx, userID, taskID, limit, offset)
}

func (s *ScheduledTaskService) Update(ctx context.Context, userID, taskID string, input UpdateScheduledTaskInput) (*model.ScheduledTask, error) {
	task, err := s.Get(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if input.Name != nil {
		if err := s.validateText(*input.Name, task.Prompt); err != nil {
			return nil, err
		}
		task.Name = strings.TrimSpace(*input.Name)
	}
	if input.Prompt != nil {
		if err := s.validateText(task.Name, *input.Prompt); err != nil {
			return nil, err
		}
		task.Prompt = *input.Prompt
	}

	scheduleChanged := input.Schedule != nil
	if scheduleChanged {
		if err := schedule.Validate(*input.Schedule, now, true); err != nil {
			return nil, scheduledTaskValidation("INVALID_SCHEDULE", err.Error())
		}
		next, err := schedule.Next(*input.Schedule, now)
		if err != nil {
			return nil, scheduledTaskValidation("INVALID_SCHEDULE", err.Error())
		}
		task.SetSchedule(*input.Schedule)
		task.NextRunAt = scheduledTaskTime(next)
	}
	if input.Session != nil {
		if err := s.applySession(ctx, userID, task, *input.Session); err != nil {
			return nil, err
		}
	}
	if input.Model != nil {
		task.Model = strings.TrimSpace(*input.Model)
	}
	if input.ReasoningEffort != nil {
		task.ReasoningEffort = strings.TrimSpace(*input.ReasoningEffort)
	}
	if input.Model != nil || input.ReasoningEffort != nil {
		if _, code, msg := modelselect.Resolve(s.selection, task.Model, task.ReasoningEffort); code != "" {
			return nil, scheduledTaskValidation(code, msg)
		}
	}

	wasEnabled := task.Enabled
	if input.Enabled != nil {
		task.Enabled = *input.Enabled
		if !task.Enabled {
			task.PausedReason = model.ScheduledReasonTaskPaused
		} else {
			task.PausedReason = ""
			if !wasEnabled && !scheduleChanged && task.ScheduleType == schedule.TypeCron {
				next, err := schedule.Next(task.Rule(), now)
				if err != nil {
					return nil, scheduledTaskValidation("INVALID_SCHEDULE", err.Error())
				}
				task.NextRunAt = scheduledTaskTime(next)
			}
			// An unconsumed at keeps its original (possibly overdue) slot. A
			// consumed at has NextRunAt nil and is never implicitly replayed.
		}
	}
	task.ApplyDefaults()
	updated, err := s.store.UpdateScheduledTask(ctx, task)
	if err != nil {
		return nil, err
	}
	if !updated {
		return nil, ErrScheduledTaskNotFound
	}
	return s.Get(ctx, userID, taskID)
}

func (s *ScheduledTaskService) Delete(ctx context.Context, userID, taskID string) error {
	deleted, err := s.store.DeleteScheduledTask(ctx, userID, taskID, s.now().UTC())
	if err != nil {
		return err
	}
	if !deleted {
		return ErrScheduledTaskNotFound
	}
	return nil
}

func (s *ScheduledTaskService) applySession(ctx context.Context, userID string, task *model.ScheduledTask, policy model.ScheduledSessionPolicy) error {
	mode := policy.Mode
	if mode == "" {
		mode = model.ScheduledSessionNew
	}
	switch mode {
	case model.ScheduledSessionNew:
		if strings.TrimSpace(policy.SessionID) != "" {
			return scheduledTaskValidation("INVALID_SESSION", "new session mode must not include session_id")
		}
		task.SessionMode = mode
		task.SessionID = nil
	case model.ScheduledSessionExisting:
		sessionID := strings.TrimSpace(policy.SessionID)
		if sessionID == "" {
			return scheduledTaskValidation("INVALID_SESSION", "existing session mode requires session_id")
		}
		sess, err := s.store.GetSessionByID(ctx, sessionID)
		if err != nil {
			return err
		}
		if sess == nil || sess.UserID != userID {
			return ErrScheduledTaskNotFound
		}
		task.SessionMode = mode
		task.SessionID = &sessionID
	default:
		return scheduledTaskValidation("INVALID_SESSION", "session mode must be new or existing")
	}
	task.Session = model.ScheduledSessionPolicy{Mode: task.SessionMode, SessionID: policy.SessionID}
	return nil
}

func (s *ScheduledTaskService) validateText(name, prompt string) error {
	if strings.TrimSpace(name) == "" {
		return scheduledTaskValidation("INVALID_NAME", "name must be non-empty")
	}
	if len([]rune(strings.TrimSpace(name))) > 255 {
		return scheduledTaskValidation("INVALID_NAME", "name must be at most 255 characters")
	}
	if strings.TrimSpace(prompt) == "" {
		return scheduledTaskValidation("INVALID_PROMPT", "prompt must be non-empty")
	}
	if len(prompt) > 1<<20 {
		return scheduledTaskValidation("CONTENT_TOO_LONG", "prompt exceeds the 1MB request limit")
	}
	if s.maxInputTokens > 0 {
		estimate := tokens.Estimate(prompt)
		if estimate > s.maxInputTokens {
			return scheduledTaskValidation("CONTENT_TOO_LONG", fmt.Sprintf("prompt token estimate %d exceeds limit %d", estimate, s.maxInputTokens))
		}
	}
	return nil
}

func scheduledTaskTime(value time.Time) *time.Time {
	value = value.UTC().Truncate(time.Millisecond)
	return &value
}

func scheduledTaskPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
