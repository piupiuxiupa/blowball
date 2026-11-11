package resource

import (
	"context"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/rpcx"
)

// SchedulerTaskOwner implements TaskOwner over the SchedulerService.
type SchedulerTaskOwner struct {
	client schedulerv1.SchedulerServiceClient
}

// NewSchedulerTaskOwner wraps the generated Scheduler client.
func NewSchedulerTaskOwner(client schedulerv1.SchedulerServiceClient) *SchedulerTaskOwner {
	return &SchedulerTaskOwner{client: client}
}

// OwnsTask reports whether the task exists for the actor. A Scheduler outage
// returns an error so callers fail closed.
func (o *SchedulerTaskOwner) OwnsTask(ctx context.Context, userID, taskID string) (bool, error) {
	_, err := o.client.GetTask(ctx, &schedulerv1.GetTaskRequest{
		Actor: &schedulerv1.Actor{UserId: userID}, TaskId: taskID,
	})
	if err != nil {
		de, ok := rpcx.AsError(rpcx.FromStatus(err))
		if ok && de.Code == rpcx.CodeNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SchedulerReferenceChecker implements ReferenceChecker for controlled
// version cleanup.
type SchedulerReferenceChecker struct {
	client schedulerv1.SchedulerServiceClient
}

// NewSchedulerReferenceChecker wraps the generated Scheduler client.
func NewSchedulerReferenceChecker(client schedulerv1.SchedulerServiceClient) *SchedulerReferenceChecker {
	return &SchedulerReferenceChecker{client: client}
}

// ResourceReferenced asks the Scheduler whether the revision is still pinned.
func (c *SchedulerReferenceChecker) ResourceReferenced(ctx context.Context, taskID string, revision uint64) (bool, error) {
	resp, err := c.client.GetResourceReferences(ctx, &schedulerv1.GetResourceReferencesRequest{
		TaskId: taskID, ResourceRevision: revision,
	})
	if err != nil {
		return false, err
	}
	return resp.GetReferenced(), nil
}
