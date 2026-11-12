// Package task owns the Scheduler's task-definition business: validation,
// create/read/update/delete, revision CAS and immutable-snapshot materialization
// inputs. It never constructs an Agent or touches a user workspace.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/internal/scheduledcontract"
	"github.com/lush/blowball/internal/tokens"
)

var (
	// ErrNotFound is the owner-safe missing-task result.
	ErrNotFound = errors.New("scheduled task not found")
	// ErrRevisionConflict is the shared optimistic-concurrency conflict.
	ErrRevisionConflict = scheduledcontract.ErrRevisionConflict
)

// ValidationError is a client-visible 400 with a stable code.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(code, format string, args ...any) error {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidateConfig is the non-secret configuration re-validated by the main
// service (owner active, model catalog, resource binding).
type ValidateConfig struct {
	Name             string
	Prompt           string
	Schedule         schedule.Rule
	Model            string
	ReasoningEffort  string
	ResourceRevision uint64
	ManifestHash     string
}

// ConfigValidator is implemented by the main-service control RPC client.
type ConfigValidator interface {
	ValidateTaskConfig(ctx context.Context, actorUserID, taskID string, cfg ValidateConfig) (resolvedModel, resolvedEffort string, err error)
}

// Store is the Scheduler's persistence surface.
type Store interface {
	CreateTask(ctx context.Context, task *model.ScheduledTask) error
	GetTask(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error)
	ListTasks(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error)
	UpdateTask(ctx context.Context, task *model.ScheduledTask, expectedRevision int64) (int64, error)
	DeleteTask(ctx context.Context, userID, taskID string, now time.Time) (bool, error)
	ListOccurrences(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error)
	GetOccurrence(ctx context.Context, occurrenceID string) (*model.ScheduledTaskRun, error)
	ResourceReferences(ctx context.Context, taskID string, revision uint64) (bool, error)
}

// CreateInput is the owner-facing create request.
type CreateInput struct {
	ActorUserID      string
	Name             string
	Prompt           string
	Schedule         schedule.Rule
	Model            string
	ReasoningEffort  string
	Enabled          *bool
	ResourceRevision uint64
	ManifestHash     string
}

// UpdateInput is a partial update; expected_revision is optional (0 keeps the
// legacy transactional behavior).
type UpdateInput struct {
	ActorUserID      string
	TaskID           string
	Name             *string
	Prompt           *string
	Schedule         *schedule.Rule
	Model            *string
	ReasoningEffort  *string
	Enabled          *bool
	ResourceRevision *uint64
	ManifestHash     *string
	ExpectedRevision int64
}

// Service owns task business logic.
type Service struct {
	store          Store
	validator      ConfigValidator
	maxInputTokens int
	now            func() time.Time
}

// New wires the service. now is injectable for deterministic schedule tests.
func New(store Store, validator ConfigValidator, maxInputTokens int, clocks ...func() time.Time) *Service {
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	return &Service{store: store, validator: validator, maxInputTokens: maxInputTokens, now: now}
}

// Create validates and persists a new task.
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.ScheduledTask, error) {
	if err := s.validateText(in.Name, in.Prompt); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if err := schedule.Validate(in.Schedule, now, true); err != nil {
		return nil, invalid("INVALID_SCHEDULE", "%s", err.Error())
	}
	next, err := schedule.Next(in.Schedule, now)
	if err != nil {
		return nil, invalid("INVALID_SCHEDULE", "%s", err.Error())
	}
	if err := validateResourcePair(in.ResourceRevision, in.ManifestHash); err != nil {
		return nil, err
	}
	resolvedModel, resolvedEffort, err := s.validateWithControl(ctx, in.ActorUserID, "", ValidateConfig{
		Name: in.Name, Prompt: in.Prompt, Schedule: in.Schedule,
		Model: in.Model, ReasoningEffort: in.ReasoningEffort,
		ResourceRevision: in.ResourceRevision, ManifestHash: in.ManifestHash,
	})
	if err != nil {
		return nil, err
	}
	taskID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("scheduled task: mint id: %w", err)
	}
	task := &model.ScheduledTask{
		TaskID: taskID.String(), UserID: in.ActorUserID,
		Name: strings.TrimSpace(in.Name), Prompt: in.Prompt,
		Model: resolvedModel, ReasoningEffort: resolvedEffort,
		Enabled: true, Revision: 1, NextRunAt: timePtr(next),
		ResourceRevision: in.ResourceRevision, ManifestHash: in.ManifestHash,
		SessionMode: model.ScheduledSessionNew,
	}
	if in.Enabled != nil {
		task.Enabled = *in.Enabled
	}
	if !task.Enabled {
		task.PausedReason = model.ScheduledReasonTaskPaused
		if in.Schedule.Type == schedule.TypeCron {
			task.NextRunAt = nil
		}
	}
	task.SetSchedule(in.Schedule)
	task.ApplyDefaults()
	if err := s.store.CreateTask(ctx, task); err != nil {
		return nil, err
	}
	return s.Get(ctx, in.ActorUserID, task.TaskID)
}

// Get returns one live owner task.
func (s *Service) Get(ctx context.Context, userID, taskID string) (*model.ScheduledTask, error) {
	task, err := s.store.GetTask(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrNotFound
	}
	return task, nil
}

// List returns one owner page.
func (s *Service) List(ctx context.Context, userID string, limit, offset int) ([]model.ScheduledTask, error) {
	return s.store.ListTasks(ctx, userID, limit, offset)
}

// Update applies a partial configuration edit. The server-owned execution hold
// is never modified here.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*model.ScheduledTask, error) {
	task, err := s.Get(ctx, in.ActorUserID, in.TaskID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if in.Name != nil {
		if err := s.validateText(*in.Name, task.Prompt); err != nil {
			return nil, err
		}
		task.Name = strings.TrimSpace(*in.Name)
	}
	if in.Prompt != nil {
		if err := s.validateText(task.Name, *in.Prompt); err != nil {
			return nil, err
		}
		task.Prompt = *in.Prompt
	}

	scheduleChanged := in.Schedule != nil
	if scheduleChanged {
		if err := schedule.Validate(*in.Schedule, now, true); err != nil {
			return nil, invalid("INVALID_SCHEDULE", "%s", err.Error())
		}
		next, err := schedule.Next(*in.Schedule, now)
		if err != nil {
			return nil, invalid("INVALID_SCHEDULE", "%s", err.Error())
		}
		task.SetSchedule(*in.Schedule)
		task.NextRunAt = timePtr(next)
	}
	if in.Model != nil {
		task.Model = strings.TrimSpace(*in.Model)
	}
	if in.ReasoningEffort != nil {
		task.ReasoningEffort = strings.TrimSpace(*in.ReasoningEffort)
	}

	// Resources are updated as a pair; clearing requires revision 0 + empty
	// hash. A task never accepts a draft/latest path — only a published version.
	resourceRevision := task.ResourceRevision
	manifestHash := task.ManifestHash
	if in.ResourceRevision != nil {
		resourceRevision = *in.ResourceRevision
	}
	if in.ManifestHash != nil {
		manifestHash = strings.TrimSpace(*in.ManifestHash)
	}
	if in.ResourceRevision != nil || in.ManifestHash != nil {
		if err := validateResourcePair(resourceRevision, manifestHash); err != nil {
			return nil, err
		}
		task.ResourceRevision = resourceRevision
		task.ManifestHash = manifestHash
	}

	wasEnabled := task.Enabled
	if in.Enabled != nil {
		task.Enabled = *in.Enabled
		if !task.Enabled {
			task.PausedReason = model.ScheduledReasonTaskPaused
			if task.ScheduleType == schedule.TypeCron {
				task.NextRunAt = nil
			}
		} else {
			task.PausedReason = ""
			if !wasEnabled && !scheduleChanged && task.ScheduleType == schedule.TypeCron {
				next, err := schedule.Next(task.Rule(), now)
				if err != nil {
					return nil, invalid("INVALID_SCHEDULE", "%s", err.Error())
				}
				task.NextRunAt = timePtr(next)
			}
			// An unconsumed at keeps its slot; a consumed at (NextRunAt nil)
			// is never implicitly replayed.
		}
	}

	// Every update migrates a legacy existing-session policy to the new
	// per-occurrence policy. Past ordinary conversations are never touched.
	task.SessionMode = model.ScheduledSessionNew
	task.SessionID = nil
	task.Session = model.ScheduledSessionPolicy{Mode: model.ScheduledSessionNew}

	resolvedModel, resolvedEffort, err := s.validateWithControl(ctx, in.ActorUserID, task.TaskID, ValidateConfig{
		Name: task.Name, Prompt: task.Prompt, Schedule: task.Rule(),
		Model: task.Model, ReasoningEffort: task.ReasoningEffort,
		ResourceRevision: task.ResourceRevision, ManifestHash: task.ManifestHash,
	})
	if err != nil {
		return nil, err
	}
	task.Model, task.ReasoningEffort = resolvedModel, resolvedEffort
	task.ApplyDefaults()

	if _, err := s.store.UpdateTask(ctx, task, in.ExpectedRevision); err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			return nil, ErrRevisionConflict
		}
		return nil, err
	}
	return s.Get(ctx, in.ActorUserID, in.TaskID)
}

// Delete soft-deletes the task and cancels unstarted occurrences.
func (s *Service) Delete(ctx context.Context, userID, taskID string) error {
	deleted, err := s.store.DeleteTask(ctx, userID, taskID, s.now().UTC())
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// Occurrences lists a task's history. A soft-deleted task keeps its recorded
// facts readable because the query is owner-scoped directly on the runs.
func (s *Service) Occurrences(ctx context.Context, userID, taskID string, limit, offset int) ([]model.ScheduledTaskRun, error) {
	return s.store.ListOccurrences(ctx, userID, taskID, limit, offset)
}

// GetOccurrence reads the authoritative occurrence (Scheduler-internal).
func (s *Service) GetOccurrence(ctx context.Context, occurrenceID string) (*model.ScheduledTaskRun, error) {
	run, err := s.store.GetOccurrence(ctx, occurrenceID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	return run, nil
}

// ResourceReferenced reports whether a published revision is still pinned by
// the live task or any retained occurrence.
func (s *Service) ResourceReferenced(ctx context.Context, taskID string, revision uint64) (bool, error) {
	return s.store.ResourceReferences(ctx, taskID, revision)
}

func (s *Service) validateWithControl(ctx context.Context, actor, taskID string, cfg ValidateConfig) (string, string, error) {
	if s.validator == nil {
		return "", "", errors.New("scheduled task: main-service validation is unavailable")
	}
	resolvedModel, resolvedEffort, err := s.validator.ValidateTaskConfig(ctx, actor, taskID, cfg)
	if err != nil {
		return "", "", err
	}
	return resolvedModel, resolvedEffort, nil
}

func (s *Service) validateText(name, prompt string) error {
	if strings.TrimSpace(name) == "" {
		return invalid("INVALID_NAME", "name must be non-empty")
	}
	if len([]rune(strings.TrimSpace(name))) > 255 {
		return invalid("INVALID_NAME", "name must be at most 255 characters")
	}
	if strings.TrimSpace(prompt) == "" {
		return invalid("INVALID_PROMPT", "prompt must be non-empty")
	}
	if len(prompt) > 1<<20 {
		return invalid("CONTENT_TOO_LONG", "prompt exceeds the 1MB request limit")
	}
	if s.maxInputTokens > 0 {
		estimate := tokens.Estimate(prompt)
		if estimate > s.maxInputTokens {
			return invalid("CONTENT_TOO_LONG", "prompt token estimate %d exceeds limit %d", estimate, s.maxInputTokens)
		}
	}
	return nil
}

func validateResourcePair(revision uint64, hash string) error {
	hash = strings.TrimSpace(hash)
	if revision == 0 && hash != "" {
		return invalid("INVALID_RESOURCE", "manifest_hash requires a non-zero resource_revision")
	}
	if revision > 0 && hash == "" {
		return invalid("INVALID_RESOURCE", "resource_revision requires a manifest_hash")
	}
	return nil
}

func timePtr(t time.Time) *time.Time {
	v := t.UTC().Truncate(time.Millisecond)
	return &v
}
