// Package rpc implements the versioned SchedulerService. It converts transport
// DTOs to the task/scheduler ports and maps domain errors to the shared status
// contract. All business authority stays in the task and scheduler packages.
package rpc

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/schedule"
	"github.com/lush/blowball/services/scheduler/internal/scheduler"
	"github.com/lush/blowball/services/scheduler/internal/store"
	"github.com/lush/blowball/services/scheduler/internal/task"
)

// Server implements schedulerv1.SchedulerServiceServer.
type Server struct {
	schedulerv1.UnimplementedSchedulerServiceServer
	tasks *task.Service
	sched *scheduler.Scheduler
}

// NewServer wires the RPC surface.
func NewServer(tasks *task.Service, sched *scheduler.Scheduler) *Server {
	return &Server{tasks: tasks, sched: sched}
}

// CreateTask implements the owner-facing create.
func (s *Server) CreateTask(ctx context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	cfg := req.GetConfig()
	rule, err := ruleFromProto(cfg.GetSchedule())
	if err != nil {
		return nil, err
	}
	created, err := s.tasks.Create(ctx, task.CreateInput{
		ActorUserID:      actor,
		Name:             cfg.GetName(),
		Prompt:           cfg.GetPrompt(),
		Schedule:         rule,
		Model:            cfg.GetModel(),
		ReasoningEffort:  cfg.GetReasoningEffort(),
		Enabled:          boolPtr(cfg.GetEnabled()),
		ResourceRevision: cfg.GetResourceRevision(),
		ManifestHash:     cfg.GetManifestHash(),
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.CreateTaskResponse{Task: taskProto(created)}, nil
}

// GetTask returns one live owner task.
func (s *Server) GetTask(ctx context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	t, err := s.tasks.Get(ctx, actor, req.GetTaskId())
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.GetTaskResponse{Task: taskProto(t)}, nil
}

// ListTasks returns one owner page.
func (s *Server) ListTasks(ctx context.Context, req *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	tasks, err := s.tasks.List(ctx, actor, int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, rpcErr(err)
	}
	out := make([]*schedulerv1.Task, 0, len(tasks))
	for i := range tasks {
		out = append(out, taskProto(&tasks[i]))
	}
	return &schedulerv1.ListTasksResponse{Tasks: out}, nil
}

// UpdateTask replaces the task configuration (full config; expected_revision
// provides optimistic concurrency).
func (s *Server) UpdateTask(ctx context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	cfg := req.GetConfig()
	rule, err := ruleFromProto(cfg.GetSchedule())
	if err != nil {
		return nil, err
	}
	name, prompt := cfg.GetName(), cfg.GetPrompt()
	modelName, effort := cfg.GetModel(), cfg.GetReasoningEffort()
	revision, hash := cfg.GetResourceRevision(), cfg.GetManifestHash()
	enabled := cfg.GetEnabled()
	updated, err := s.tasks.Update(ctx, task.UpdateInput{
		ActorUserID: actor, TaskID: req.GetTaskId(), ExpectedRevision: req.GetExpectedRevision(),
		Name: &name, Prompt: &prompt, Schedule: &rule, Model: &modelName,
		ReasoningEffort: &effort, Enabled: &enabled,
		ResourceRevision: &revision, ManifestHash: &hash,
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.UpdateTaskResponse{Task: taskProto(updated)}, nil
}

// DeleteTask soft-deletes a task.
func (s *Server) DeleteTask(ctx context.Context, req *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	t, err := s.tasks.Get(ctx, actor, req.GetTaskId())
	if err != nil {
		return nil, rpcErr(err)
	}
	if req.GetExpectedRevision() > 0 && req.GetExpectedRevision() != t.Revision {
		return nil, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeRevisionConflict, "scheduled task revision conflict"))
	}
	if err := s.tasks.Delete(ctx, actor, req.GetTaskId()); err != nil {
		return nil, rpcErr(err)
	}
	resp := taskProto(t)
	resp.Deleted = true
	return &schedulerv1.DeleteTaskResponse{Task: resp}, nil
}

// ListOccurrences returns recorded execution history; a soft-deleted task's
// facts remain readable.
func (s *Server) ListOccurrences(ctx context.Context, req *schedulerv1.ListOccurrencesRequest) (*schedulerv1.ListOccurrencesResponse, error) {
	actor, err := actorOf(req.GetActor())
	if err != nil {
		return nil, err
	}
	runs, err := s.tasks.Occurrences(ctx, actor, req.GetTaskId(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, rpcErr(err)
	}
	out := make([]*schedulerv1.Occurrence, 0, len(runs))
	for i := range runs {
		out = append(out, occurrenceProto(&runs[i]))
	}
	return &schedulerv1.ListOccurrencesResponse{Occurrences: out}, nil
}

// GetOccurrence returns the authoritative occurrence to the Runtime.
func (s *Server) GetOccurrence(ctx context.Context, req *schedulerv1.GetOccurrenceRequest) (*schedulerv1.GetOccurrenceResponse, error) {
	run, err := s.tasks.GetOccurrence(ctx, req.GetOccurrenceId())
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.GetOccurrenceResponse{Occurrence: occurrenceProto(run)}, nil
}

// AuthorizeStart persists/returns the unique start grant.
func (s *Server) AuthorizeStart(ctx context.Context, req *schedulerv1.AuthorizeStartRequest) (*schedulerv1.AuthorizeStartResponse, error) {
	if req.GetOccurrenceId() == "" || req.GetRunId() == "" || req.GetSessionId() == "" || req.GetReceiptGeneration() == 0 {
		return nil, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "occurrence_id, run_id, session_id and receipt_generation are required"))
	}
	grant, err := s.sched.AuthorizeStart(ctx, req.GetOccurrenceId(), req.GetRunId(), req.GetSessionId(),
		int(req.GetAttempt()), req.GetReceiptGeneration(), req.GetRuntimeWorkerGeneration())
	if err != nil {
		logger.FromContext(ctx).Warn("scheduled start authorization rejected",
			zap.String("op", "scheduler.authorize_start"),
			zap.String("occurrence_id", req.GetOccurrenceId()),
			zap.Uint64("receipt_generation", req.GetReceiptGeneration()),
			zap.Error(err))
		return nil, rpcErr(err)
	}
	logger.FromContext(ctx).Info("scheduled start granted",
		zap.String("op", "scheduler.authorize_start"),
		zap.String("occurrence_id", grant.OccurrenceID),
		zap.String("run_id", grant.RunID),
		zap.Uint64("generation", grant.Generation),
		zap.Time("deadline", grant.Deadline))
	return &schedulerv1.AuthorizeStartResponse{Grant: grantProto(grant)}, nil
}

// GetStartGrant resolves an idempotent grant after an ambiguous response.
func (s *Server) GetStartGrant(ctx context.Context, req *schedulerv1.GetStartGrantRequest) (*schedulerv1.GetStartGrantResponse, error) {
	grant, err := s.sched.GetStartGrant(ctx, req.GetOccurrenceId(), req.GetReceiptGeneration())
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.GetStartGrantResponse{Grant: grantProto(grant)}, nil
}

// GetResourceReferences answers the main service's cleanup reference check.
func (s *Server) GetResourceReferences(ctx context.Context, req *schedulerv1.GetResourceReferencesRequest) (*schedulerv1.GetResourceReferencesResponse, error) {
	if req.GetTaskId() == "" || req.GetResourceRevision() == 0 {
		return nil, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "task_id and resource_revision are required"))
	}
	referenced, err := s.tasks.ResourceReferenced(ctx, req.GetTaskId(), req.GetResourceRevision())
	if err != nil {
		return nil, rpcErr(err)
	}
	return &schedulerv1.GetResourceReferencesResponse{Referenced: referenced}, nil
}

func actorOf(actor *schedulerv1.Actor) (string, error) {
	if actor == nil || actor.GetUserId() == "" {
		return "", rpcx.ToStatus(rpcx.Errorf(rpcx.CodeUnauthenticated, "actor is required"))
	}
	return actor.GetUserId(), nil
}

func boolPtr(v bool) *bool { return &v }

func ruleFromProto(s *schedulerv1.Schedule) (schedule.Rule, error) {
	if s == nil {
		return schedule.Rule{}, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "schedule is required"))
	}
	rule := schedule.Rule{Expression: s.GetCronExpression(), Timezone: s.GetTimezone()}
	switch s.GetType() {
	case schedulerv1.ScheduleType_SCHEDULE_TYPE_AT:
		rule.Type = schedule.TypeAt
		rule.RunAt = s.GetRunAt()
	case schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON:
		rule.Type = schedule.TypeCron
	default:
		return schedule.Rule{}, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "schedule type is required"))
	}
	return rule, nil
}

func taskProto(t *model.ScheduledTask) *schedulerv1.Task {
	if t == nil {
		return nil
	}
	cfg := &schedulerv1.TaskConfig{
		Name: t.Name, Prompt: t.Prompt,
		Schedule: &schedulerv1.Schedule{
			Type:           scheduleTypeProto(t.Schedule.Type),
			RunAt:          t.Schedule.RunAt,
			CronExpression: t.Schedule.Expression,
			Timezone:       t.Schedule.Timezone,
		},
		Model: t.Model, ReasoningEffort: t.ReasoningEffort, Enabled: t.Enabled,
		ResourceRevision: t.ResourceRevision, ManifestHash: t.ManifestHash,
	}
	return &schedulerv1.Task{
		TaskId: t.TaskID, UserId: t.UserID, Config: cfg, Revision: t.Revision,
		NextRunAt: tsPtr(t.NextRunAt), PausedReason: t.PausedReason,
		ExecutionHoldOccurrenceId: t.ExecutionHoldOccurrenceID, BlockedReason: t.BlockedReason,
		CreateTime: timestamppb.New(t.CreateTime), UpdateTime: timestamppb.New(t.UpdateTime),
		Deleted: t.DeletedAt != nil,
	}
}

func occurrenceProto(r *model.ScheduledTaskRun) *schedulerv1.Occurrence {
	if r == nil {
		return nil
	}
	return &schedulerv1.Occurrence{
		OccurrenceId: r.OccurrenceID, TaskId: r.TaskID, UserId: r.UserID,
		TaskRevision: r.Revision, SnapshotJson: r.SnapshotJSON,
		ScheduledFor: timestamppb.New(r.ScheduledFor), RunId: r.RunID, SessionId: r.SessionID,
		SessionSource: sessionSourceProto(r.SessionSource), Status: r.Status, Reason: r.Reason,
		AttemptCount: uint32(r.AttemptCount), StartedAt: tsPtr(r.StartedAt), FinishedAt: tsPtr(r.FinishedAt),
		LastError: r.LastError, ResourceRevision: r.ResourceRevision, ManifestHash: r.ManifestHash,
		UpdateTime: timestamppb.New(r.UpdateTime), CreateTime: timestamppb.New(r.CreateTime),
	}
}

func grantProto(g *model.ScheduledStartGrant) *schedulerv1.StartGrant {
	if g == nil {
		return nil
	}
	return &schedulerv1.StartGrant{
		OccurrenceId: g.OccurrenceID, Generation: g.Generation, RunId: g.RunID, SessionId: g.SessionID,
		StartedAt: timestamppb.New(g.StartedAt), Deadline: timestamppb.New(g.Deadline),
	}
}

func scheduleTypeProto(t string) schedulerv1.ScheduleType {
	switch t {
	case schedule.TypeAt:
		return schedulerv1.ScheduleType_SCHEDULE_TYPE_AT
	case schedule.TypeCron:
		return schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON
	default:
		return schedulerv1.ScheduleType_SCHEDULE_TYPE_UNSPECIFIED
	}
}

func sessionSourceProto(s string) schedulerv1.SessionSource {
	switch s {
	case string(model.SessionSourceScheduled):
		return schedulerv1.SessionSource_SESSION_SOURCE_SCHEDULED
	case string(model.SessionSourceLegacyInteractive):
		return schedulerv1.SessionSource_SESSION_SOURCE_LEGACY_INTERACTIVE
	default:
		return schedulerv1.SessionSource_SESSION_SOURCE_UNSPECIFIED
	}
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}

func rpcErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := rpcx.AsError(err); ok {
		return rpcx.ToStatus(err)
	}
	var ve *task.ValidationError
	if errors.As(err, &ve) {
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "%s", ve.Message))
	}
	switch {
	case errors.Is(err, task.ErrNotFound), errors.Is(err, store.ErrOccurrenceNotFound),
		errors.Is(err, store.ErrGrantNotFound):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeNotFound, "%s", err.Error()))
	case errors.Is(err, task.ErrRevisionConflict):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeRevisionConflict, "%s", err.Error()))
	case errors.Is(err, store.ErrTaskUnavailable), errors.Is(err, store.ErrGrantFenced):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeStartRejected, "%s", err.Error()))
	default:
		return rpcx.ToStatus(rpcx.Wrap(rpcx.CodeUnavailable, "scheduler persistence error", err))
	}
}
