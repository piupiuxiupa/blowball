package rpcclient

import (
	"context"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/schedule"
	"github.com/lush/blowball/services/scheduler/internal/task"
)

// ControlClient implements task.ConfigValidator over the main service's
// SchedulerControlService. It never validates locally: owner, model catalog
// and resource binding are main-service authority.
type ControlClient struct {
	client schedulerv1.SchedulerControlServiceClient
}

// NewControlClient wraps the generated client.
func NewControlClient(client schedulerv1.SchedulerControlServiceClient) *ControlClient {
	return &ControlClient{client: client}
}

// ValidateTaskConfig re-validates the owner, model/effort and resource binding.
func (c *ControlClient) ValidateTaskConfig(ctx context.Context, actorUserID, taskID string, cfg task.ValidateConfig) (string, string, error) {
	resp, err := c.client.ValidateTaskActorAndConfig(ctx, &schedulerv1.ValidateTaskActorAndConfigRequest{
		Actor:  &schedulerv1.Actor{UserId: actorUserID},
		TaskId: taskID,
		Config: &schedulerv1.TaskConfig{
			Name:             cfg.Name,
			Prompt:           cfg.Prompt,
			Schedule:         scheduleProto(cfg.Schedule),
			Model:            cfg.Model,
			ReasoningEffort:  cfg.ReasoningEffort,
			Enabled:          true,
			ResourceRevision: cfg.ResourceRevision,
			ManifestHash:     cfg.ManifestHash,
		},
	})
	if err != nil {
		return "", "", rpcx.FromStatus(err)
	}
	return resp.GetResolvedModel(), resp.GetResolvedReasoningEffort(), nil
}

func scheduleProto(rule schedule.Rule) *schedulerv1.Schedule {
	out := &schedulerv1.Schedule{
		CronExpression: rule.Expression,
		Timezone:       rule.Timezone,
	}
	switch rule.Type {
	case schedule.TypeAt:
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_AT
		out.RunAt = rule.RunAt
	case schedule.TypeCron:
		out.Type = schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON
	}
	return out
}
