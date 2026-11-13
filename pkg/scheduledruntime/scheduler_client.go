package scheduledruntime

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/rpcx"
)

// SchedulerClient adapts the versioned SchedulerService client to the
// Runtime's neutral occurrence/authorization surface. It holds no credential.
type SchedulerClient struct {
	client schedulerv1.SchedulerServiceClient
}

// NewSchedulerClient wraps the generated client.
func NewSchedulerClient(client schedulerv1.SchedulerServiceClient) *SchedulerClient {
	return &SchedulerClient{client: client}
}

// GetOccurrence fetches the authoritative occurrence.
func (c *SchedulerClient) GetOccurrence(ctx context.Context, occurrenceID string) (*Occurrence, error) {
	resp, err := c.client.GetOccurrence(ctx, &schedulerv1.GetOccurrenceRequest{OccurrenceId: occurrenceID})
	if err != nil {
		return nil, rpcx.FromStatus(err)
	}
	o := resp.GetOccurrence()
	if o == nil {
		return nil, nil
	}
	return &Occurrence{
		OccurrenceID:     o.GetOccurrenceId(),
		TaskID:           o.GetTaskId(),
		UserID:           o.GetUserId(),
		TaskRevision:     o.GetTaskRevision(),
		SnapshotJSON:     o.GetSnapshotJson(),
		RunID:            o.GetRunId(),
		SessionID:        o.GetSessionId(),
		SessionSource:    sessionSourceString(o.GetSessionSource()),
		Status:           o.GetStatus(),
		AttemptCount:     int(o.GetAttemptCount()),
		ResourceRevision: o.GetResourceRevision(),
		ManifestHash:     o.GetManifestHash(),
		ScheduledFor:     asTime(o.GetScheduledFor()),
	}, nil
}

// AuthorizeStart asks the Scheduler for the unique start grant.
func (c *SchedulerClient) AuthorizeStart(ctx context.Context, req AuthorizeStartRequest) (*Grant, error) {
	resp, err := c.client.AuthorizeStart(ctx, &schedulerv1.AuthorizeStartRequest{
		OccurrenceId:            req.OccurrenceID,
		RunId:                   req.RunID,
		SessionId:               req.SessionID,
		Attempt:                 uint32(req.Attempt),
		ReceiptGeneration:       req.ReceiptGeneration,
		ClaimFenceId:            req.ClaimFenceID,
		RuntimeWorkerGeneration: req.RuntimeWorkerGeneration,
	})
	if err != nil {
		return nil, rpcx.FromStatus(err)
	}
	return grantFromProto(resp.GetGrant()), nil
}

// GetStartGrant resolves an idempotent existing grant after an ambiguous
// authorization response.
func (c *SchedulerClient) GetStartGrant(ctx context.Context, occurrenceID string, generation uint64) (*Grant, error) {
	resp, err := c.client.GetStartGrant(ctx, &schedulerv1.GetStartGrantRequest{
		OccurrenceId:      occurrenceID,
		ReceiptGeneration: generation,
	})
	if err != nil {
		return nil, rpcx.FromStatus(err)
	}
	return grantFromProto(resp.GetGrant()), nil
}

func grantFromProto(g *schedulerv1.StartGrant) *Grant {
	if g == nil {
		return nil
	}
	return &Grant{
		OccurrenceID: g.GetOccurrenceId(),
		Generation:   g.GetGeneration(),
		RunID:        g.GetRunId(),
		SessionID:    g.GetSessionId(),
		StartedAt:    asTime(g.GetStartedAt()),
		Deadline:     asTime(g.GetDeadline()),
	}
}

func asTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

func sessionSourceString(src schedulerv1.SessionSource) string {
	switch src {
	case schedulerv1.SessionSource_SESSION_SOURCE_SCHEDULED:
		return "scheduled"
	case schedulerv1.SessionSource_SESSION_SOURCE_LEGACY_INTERACTIVE:
		return "legacy_interactive"
	default:
		return ""
	}
}
