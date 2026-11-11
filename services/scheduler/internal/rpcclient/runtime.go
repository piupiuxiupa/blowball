// Package rpcclient adapts the versioned generation clients to the Scheduler's
// narrow ports. It holds no credential and performs no business validation.
package rpcclient

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	"github.com/lush/blowball/internal/rpcx"
	sched "github.com/lush/blowball/services/scheduler/internal/scheduler"
)

// RuntimeClient implements scheduler.RuntimeClient over the Agent/all
// ScheduledRuntimeService.
type RuntimeClient struct {
	client scheduledruntimev1.ScheduledRuntimeServiceClient
}

// NewRuntimeClient wraps the generated client.
func NewRuntimeClient(client scheduledruntimev1.ScheduledRuntimeServiceClient) *RuntimeClient {
	return &RuntimeClient{client: client}
}

// SubmitScheduledRun performs the idempotent acceptance call.
func (c *RuntimeClient) SubmitScheduledRun(ctx context.Context, in sched.SubmitInput) (*sched.SubmitResult, error) {
	resp, err := c.client.SubmitScheduledRun(ctx, &scheduledruntimev1.SubmitScheduledRunRequest{
		OccurrenceId:      in.OccurrenceID,
		TaskId:            in.TaskID,
		RunId:             in.RunID,
		SessionId:         in.SessionID,
		Attempt:           uint32(in.Attempt),
		ConfigFingerprint: in.ConfigFingerprint,
	})
	if err != nil {
		return nil, rpcx.FromStatus(err)
	}
	return &sched.SubmitResult{
		ReceiptGeneration: resp.GetReceiptGeneration(),
		Status:            resp.GetStatus(),
		AlreadyAccepted:   resp.GetAlreadyAccepted(),
	}, nil
}

// GetScheduledExecution reads the authoritative fact. A missing execution
// returns (nil, nil) so the caller can distinguish "not accepted" from an
// unreachable Runtime.
func (c *RuntimeClient) GetScheduledExecution(ctx context.Context, occurrenceID string) (*sched.ExecutionFact, error) {
	resp, err := c.client.GetScheduledExecution(ctx, &scheduledruntimev1.GetScheduledExecutionRequest{
		OccurrenceId: occurrenceID,
	})
	if err != nil {
		if de, ok := rpcx.AsError(rpcx.FromStatus(err)); ok && de.Code == rpcx.CodeNotFound {
			return nil, nil
		}
		return nil, rpcx.FromStatus(err)
	}
	e := resp.GetExecution()
	if e == nil {
		return nil, nil
	}
	return &sched.ExecutionFact{
		Status:              e.GetStatus(),
		Reason:              e.GetReason(),
		LastError:           e.GetLastError(),
		ReceiptGeneration:   e.GetReceiptGeneration(),
		Terminal:            e.GetTerminal(),
		Started:             e.GetStarted(),
		Uncertain:           e.GetUncertain(),
		StartedAt:           asTimePtr(e.GetStartedAt()),
		FinishedAt:          asTimePtr(e.GetFinishedAt()),
		GrantGeneration:     e.GetGrantGeneration(),
		GrantDeadline:       asTimePtr(e.GetGrantDeadline()),
		StopProofPresent:    e.GetStopProof().GetPresent(),
		StopProofGeneration: e.GetStopProof().GetGeneration(),
	}, nil
}

func asTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
}
