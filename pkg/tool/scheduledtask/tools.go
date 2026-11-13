// Package scheduledtask exposes the Agent's scheduled-task management tools.
// Every tool derives its actor from the trusted execution context, calls the
// same Scheduler RPC / main-service resource business as HTTP, and never
// exposes SQL, crontab files or arbitrary user proxying. The tools are
// registered only when the Scheduler is configured.
package scheduledtask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/resource"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/schedule"
	"github.com/lush/blowball/pkg/tool"
	"github.com/lush/blowball/pkg/tool/skill"
)

// Tool names.
const (
	NameCreate           = "scheduled_task_create"
	NameList             = "scheduled_task_list"
	NameGet              = "scheduled_task_get"
	NameUpdate           = "scheduled_task_update"
	NamePause            = "scheduled_task_pause"
	NameResume           = "scheduled_task_resume"
	NameDelete           = "scheduled_task_delete"
	NameResourcesGet     = "scheduled_task_resources_get"
	NameResourcesPublish = "scheduled_task_resources_publish"
)

// SchedulerClient is the task-management RPC subset (implemented by
// scheduledcontrol.SchedulerManagementClient).
type SchedulerClient interface {
	CreateTask(ctx context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error)
	GetTask(ctx context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error)
	ListTasks(ctx context.Context, req *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error)
	UpdateTask(ctx context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error)
	DeleteTask(ctx context.Context, req *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error)
}

// ResourceService is the main-service `.cron` resource subset.
type ResourceService interface {
	GetDraft(ctx context.Context, userID, taskID string) (*resource.DraftInfo, error)
	ListVersions(ctx context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error)
	Publish(ctx context.Context, userID, taskID string, expectedDraftRevision uint64, requestID string) (*model.ScheduledResourceVersion, error)
}

// Tools bundles the tool dependencies.
type Tools struct {
	scheduler    SchedulerClient
	resources    ResourceService
	defaultModel string
}

// NewTools wires the tool family.
func NewTools(scheduler SchedulerClient, resources ResourceService, defaultModel string) *Tools {
	return &Tools{scheduler: scheduler, resources: resources, defaultModel: defaultModel}
}

// RegisterAll registers every scheduled-task tool. A nil scheduler is a wiring
// error: callers must not register the family when the Scheduler is absent.
func RegisterAll(r *tool.Registry, t *Tools) error {
	if t == nil || t.scheduler == nil {
		return fmt.Errorf("scheduledtask: scheduler client is required")
	}
	specs := []*tool.ToolSpec{
		t.createSpec(), t.listSpec(), t.getSpec(), t.updateSpec(),
		t.pauseSpec(), t.resumeSpec(), t.deleteSpec(),
		t.resourcesGetSpec(), t.resourcesPublishSpec(),
	}
	for _, spec := range specs {
		if err := r.Register(spec); err != nil {
			return err
		}
	}
	return nil
}

func actor(ctx context.Context) (string, error) {
	userID := skill.UserIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("scheduled task tools require an authenticated execution context")
	}
	return userID, nil
}

func (t *Tools) createSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name: NameCreate,
		Description: "Creates a scheduled task owned by the current user. Resources are optional: pass a published " +
			"resource_revision + manifest_hash pair, or omit both. session.mode=existing is not supported; every " +
			"occurrence gets its own read-only task session. Returns the task with its id and revision.",
		ParametersJSON: json.RawMessage(`{
  "type":"object",
  "properties":{
    "name":{"type":"string"},
    "prompt":{"type":"string","description":"The task instruction (persisted verbatim)."},
    "schedule":{"type":"object","properties":{
      "type":{"type":"string","enum":["at","cron"]},
      "run_at":{"type":"string","description":"RFC3339 instant (at only)."},
      "expression":{"type":"string","description":"Five-field cron expression (cron only)."},
      "timezone":{"type":"string","description":"Explicit IANA timezone."}
    },"required":["type","timezone"],"additionalProperties":false},
    "model":{"type":"string"},
    "reasoning_effort":{"type":"string"},
    "enabled":{"type":"boolean"},
    "resource_revision":{"type":"integer"},
    "manifest_hash":{"type":"string"}
  },
  "required":["name","prompt","schedule"],
  "additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			var a struct {
				Name             string        `json:"name"`
				Prompt           string        `json:"prompt"`
				Schedule         schedule.Rule `json:"schedule"`
				Model            string        `json:"model"`
				ReasoningEffort  string        `json:"reasoning_effort"`
				Enabled          *bool         `json:"enabled"`
				ResourceRevision uint64        `json:"resource_revision"`
				ManifestHash     string        `json:"manifest_hash"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, fmt.Errorf("%s: parse args: %w", NameCreate, err)
			}
			resp, err := t.scheduler.CreateTask(ctx, &schedulerv1.CreateTaskRequest{
				Actor: &schedulerv1.Actor{UserId: userID},
				Config: &schedulerv1.TaskConfig{
					Name: a.Name, Prompt: a.Prompt, Schedule: scheduleProto(a.Schedule),
					Model: a.Model, ReasoningEffort: a.ReasoningEffort,
					Enabled:          a.Enabled == nil || *a.Enabled,
					ResourceRevision: a.ResourceRevision, ManifestHash: a.ManifestHash,
				},
			})
			if err != nil {
				return nil, toolError(err)
			}
			return taskView(resp.GetTask()), nil
		},
	}
}

func (t *Tools) listSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NameList,
		Description: "Lists the current user's scheduled tasks (newest first).",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"limit":{"type":"integer"},"offset":{"type":"integer"}},
  "additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			var a struct {
				Limit  int32 `json:"limit"`
				Offset int32 `json:"offset"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			resp, err := t.scheduler.ListTasks(ctx, &schedulerv1.ListTasksRequest{
				Actor: &schedulerv1.Actor{UserId: userID}, Limit: a.Limit, Offset: a.Offset,
			})
			if err != nil {
				return nil, toolError(err)
			}
			out := make([]map[string]any, 0, len(resp.GetTasks()))
			for _, task := range resp.GetTasks() {
				out = append(out, taskView(task))
			}
			return map[string]any{"tasks": out}, nil
		},
	}
}

func (t *Tools) getSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NameGet,
		Description: "Reads one scheduled task owned by the current user.",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			var a struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			resp, err := t.scheduler.GetTask(ctx, &schedulerv1.GetTaskRequest{
				Actor: &schedulerv1.Actor{UserId: userID}, TaskId: a.TaskID,
			})
			if err != nil {
				return nil, toolError(err)
			}
			return taskView(resp.GetTask()), nil
		},
	}
}

func (t *Tools) updateSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name: NameUpdate,
		Description: "Updates the current task configuration. Supply only the fields to change plus the optional " +
			"expected_revision (a stale revision fails with a conflict). Server-owned execution-hold state is never " +
			"modifiable.",
		ParametersJSON: json.RawMessage(`{
  "type":"object",
  "properties":{
    "task_id":{"type":"string"},
    "name":{"type":"string"},"prompt":{"type":"string"},
    "schedule":{"type":"object","properties":{
      "type":{"type":"string","enum":["at","cron"]},
      "run_at":{"type":"string"},"expression":{"type":"string"},"timezone":{"type":"string"}
    },"required":["type","timezone"],"additionalProperties":false},
    "model":{"type":"string"},"reasoning_effort":{"type":"string"},"enabled":{"type":"boolean"},
    "resource_revision":{"type":"integer"},"manifest_hash":{"type":"string"},
    "expected_revision":{"type":"integer"}
  },
  "required":["task_id"],
  "additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				TaskID           string         `json:"task_id"`
				Name             *string        `json:"name"`
				Prompt           *string        `json:"prompt"`
				Schedule         *schedule.Rule `json:"schedule"`
				Model            *string        `json:"model"`
				ReasoningEffort  *string        `json:"reasoning_effort"`
				Enabled          *bool          `json:"enabled"`
				ResourceRevision *uint64        `json:"resource_revision"`
				ManifestHash     *string        `json:"manifest_hash"`
				ExpectedRevision int64          `json:"expected_revision"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return t.update(ctx, a.TaskID, a.ExpectedRevision, a.Name, a.Prompt, a.Schedule, a.Model,
				a.ReasoningEffort, a.Enabled, a.ResourceRevision, a.ManifestHash)
		},
	}
}

func (t *Tools) pauseSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NamePause,
		Description: "Pauses a scheduled task (enabled=false). Running executions are not cancelled.",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			taskID, err := taskIDArg(args)
			if err != nil {
				return nil, err
			}
			disabled := false
			return t.update(ctx, taskID, 0, nil, nil, nil, nil, nil, &disabled, nil, nil)
		},
	}
}

func (t *Tools) resumeSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NameResume,
		Description: "Resumes a paused task (enabled=true). A task-level execution hold is not cleared by resuming.",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			taskID, err := taskIDArg(args)
			if err != nil {
				return nil, err
			}
			enabled := true
			return t.update(ctx, taskID, 0, nil, nil, nil, nil, nil, &enabled, nil, nil)
		},
	}
}

func (t *Tools) deleteSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NameDelete,
		Description: "Soft-deletes a scheduled task. Execution history and `.cron` resources are retained.",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			taskID, err := taskIDArg(args)
			if err != nil {
				return nil, err
			}
			_, err = t.scheduler.DeleteTask(ctx, &schedulerv1.DeleteTaskRequest{
				Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID,
			})
			if err != nil {
				return nil, toolError(err)
			}
			return map[string]any{"task_id": taskID, "deleted": true}, nil
		},
	}
}

func (t *Tools) resourcesGetSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name:        NameResourcesGet,
		Description: "Reads the task's `.cron` draft revision, draft files and published version metadata.",
		ParametersJSON: json.RawMessage(`{
  "type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			taskID, err := taskIDArg(args)
			if err != nil {
				return nil, err
			}
			if t.resources == nil {
				return nil, fmt.Errorf("scheduled task resources are unavailable")
			}
			draft, err := t.resources.GetDraft(ctx, userID, taskID)
			if err != nil {
				return nil, toolError(err)
			}
			versions, err := t.resources.ListVersions(ctx, userID, taskID)
			if err != nil {
				return nil, toolError(err)
			}
			return map[string]any{"draft": draft, "versions": versions}, nil
		},
	}
}

func (t *Tools) resourcesPublishSpec() *tool.ToolSpec {
	return &tool.ToolSpec{
		Name: NameResourcesPublish,
		Description: "Publishes the task's `.cron` draft as a new immutable resource revision. The same request_id is " +
			"idempotent; a concurrent draft edit fails with a revision conflict.",
		ParametersJSON: json.RawMessage(`{
  "type":"object",
  "properties":{
    "task_id":{"type":"string"},
    "expected_draft_revision":{"type":"integer"},
    "request_id":{"type":"string"}
  },
  "required":["task_id","request_id"],
  "additionalProperties":false
}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			userID, err := actor(ctx)
			if err != nil {
				return nil, err
			}
			var a struct {
				TaskID                string `json:"task_id"`
				ExpectedDraftRevision uint64 `json:"expected_draft_revision"`
				RequestID             string `json:"request_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			if t.resources == nil {
				return nil, fmt.Errorf("scheduled task resources are unavailable")
			}
			v, err := t.resources.Publish(ctx, userID, a.TaskID, a.ExpectedDraftRevision, a.RequestID)
			if err != nil {
				return nil, toolError(err)
			}
			return map[string]any{
				"resource_revision": v.Revision, "manifest_hash": v.ManifestHash,
				"file_count": v.FileCount, "total_bytes": v.TotalBytes,
			}, nil
		},
	}
}

func (t *Tools) update(ctx context.Context, taskID string, expected int64, name, prompt *string, rule *schedule.Rule,
	modelName, effort *string, enabled *bool, revision *uint64, hash *string) (any, error) {
	userID, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	current, err := t.scheduler.GetTask(ctx, &schedulerv1.GetTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID,
	})
	if err != nil {
		return nil, toolError(err)
	}
	cfg := current.GetTask().GetConfig()
	if name != nil {
		cfg.Name = *name
	}
	if prompt != nil {
		cfg.Prompt = *prompt
	}
	if rule != nil {
		cfg.Schedule = scheduleProto(*rule)
	}
	if modelName != nil {
		cfg.Model = *modelName
	}
	if effort != nil {
		cfg.ReasoningEffort = *effort
	}
	if enabled != nil {
		cfg.Enabled = *enabled
	}
	if revision != nil {
		cfg.ResourceRevision = *revision
	}
	if hash != nil {
		cfg.ManifestHash = strings.TrimSpace(*hash)
	}
	resp, err := t.scheduler.UpdateTask(ctx, &schedulerv1.UpdateTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID, Config: cfg, ExpectedRevision: expected,
	})
	if err != nil {
		return nil, toolError(err)
	}
	return taskView(resp.GetTask()), nil
}

func taskIDArg(args json.RawMessage) (string, error) {
	var a struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return "", fmt.Errorf("task_id is required")
	}
	return a.TaskID, nil
}

func taskView(t *schedulerv1.Task) map[string]any {
	if t == nil {
		return nil
	}
	cfg := t.GetConfig()
	return map[string]any{
		"task_id": t.GetTaskId(), "name": cfg.GetName(), "prompt": cfg.GetPrompt(),
		"schedule": scheduleRule(cfg.GetSchedule()), "model": cfg.GetModel(),
		"reasoning_effort": cfg.GetReasoningEffort(), "enabled": cfg.GetEnabled(),
		"revision": t.GetRevision(), "paused_reason": t.GetPausedReason(),
		"resource_revision": cfg.GetResourceRevision(), "manifest_hash": cfg.GetManifestHash(),
		"execution_hold_occurrence_id": t.GetExecutionHoldOccurrenceId(),
		"blocked_reason":               t.GetBlockedReason(),
	}
}

func scheduleProto(rule schedule.Rule) *schedulerv1.Schedule {
	out := &schedulerv1.Schedule{CronExpression: rule.Expression, Timezone: rule.Timezone}
	if rule.Type == schedule.TypeAt {
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_AT
		out.RunAt = rule.RunAt
	} else {
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON
	}
	return out
}

func scheduleRule(p *schedulerv1.Schedule) schedule.Rule {
	rule := schedule.Rule{Expression: p.GetCronExpression(), Timezone: p.GetTimezone()}
	if p.GetType() == schedulerv1.ScheduleType_SCHEDULE_TYPE_AT {
		rule.Type = schedule.TypeAt
		rule.RunAt = p.GetRunAt()
	} else {
		rule.Type = schedule.TypeCron
	}
	return rule
}

func toolError(err error) error {
	if de, ok := rpcx.AsError(err); ok {
		switch de.Code {
		case rpcx.CodeUnavailable:
			return fmt.Errorf("scheduled task service unavailable: %s", de.Detail)
		case rpcx.CodeNotFound:
			return fmt.Errorf("scheduled task not found")
		case rpcx.CodeRevisionConflict:
			return fmt.Errorf("scheduled task configuration changed; re-read and retry (revision conflict)")
		default:
			return fmt.Errorf("%s", de.Error())
		}
	}
	return err
}
