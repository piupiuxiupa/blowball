package scheduledcontrol

import (
	"context"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/rpcx"
)

// SchedulerManagementClient adapts the generated Scheduler client to the
// handler's option-free methods (and the occurrence lookup).
type SchedulerManagementClient struct {
	client schedulerv1.SchedulerServiceClient
}

// NewSchedulerManagementClient wraps the generated client.
func NewSchedulerManagementClient(client schedulerv1.SchedulerServiceClient) *SchedulerManagementClient {
	return &SchedulerManagementClient{client: client}
}

func (a *SchedulerManagementClient) CreateTask(ctx context.Context, req *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error) {
	return a.client.CreateTask(ctx, req)
}

func (a *SchedulerManagementClient) GetTask(ctx context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	return a.client.GetTask(ctx, req)
}

func (a *SchedulerManagementClient) ListTasks(ctx context.Context, req *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error) {
	return a.client.ListTasks(ctx, req)
}

func (a *SchedulerManagementClient) UpdateTask(ctx context.Context, req *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error) {
	return a.client.UpdateTask(ctx, req)
}

func (a *SchedulerManagementClient) DeleteTask(ctx context.Context, req *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error) {
	return a.client.DeleteTask(ctx, req)
}

func (a *SchedulerManagementClient) ListOccurrences(ctx context.Context, req *schedulerv1.ListOccurrencesRequest) (*schedulerv1.ListOccurrencesResponse, error) {
	return a.client.ListOccurrences(ctx, req)
}

// GetOccurrence implements the handler's occurrence lookup for historical
// snapshots.
func (a *SchedulerManagementClient) GetOccurrence(ctx context.Context, req *schedulerv1.GetOccurrenceRequest) (*schedulerv1.GetOccurrenceResponse, error) {
	return a.client.GetOccurrence(ctx, req)
}

// UnavailableScheduledTaskRPC is the fail-closed adapter used when the
// Scheduler is not configured. Every task endpoint returns UNAVAILABLE (503);
// there is no local fallback.
type UnavailableScheduledTaskRPC struct{}

func (UnavailableScheduledTaskRPC) CreateTask(context.Context, *schedulerv1.CreateTaskRequest) (*schedulerv1.CreateTaskResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
func (UnavailableScheduledTaskRPC) GetTask(context.Context, *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
func (UnavailableScheduledTaskRPC) ListTasks(context.Context, *schedulerv1.ListTasksRequest) (*schedulerv1.ListTasksResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
func (UnavailableScheduledTaskRPC) UpdateTask(context.Context, *schedulerv1.UpdateTaskRequest) (*schedulerv1.UpdateTaskResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
func (UnavailableScheduledTaskRPC) DeleteTask(context.Context, *schedulerv1.DeleteTaskRequest) (*schedulerv1.DeleteTaskResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
func (UnavailableScheduledTaskRPC) ListOccurrences(context.Context, *schedulerv1.ListOccurrencesRequest) (*schedulerv1.ListOccurrencesResponse, error) {
	return nil, rpcx.Errorf(rpcx.CodeUnavailable, "scheduler is not configured")
}
