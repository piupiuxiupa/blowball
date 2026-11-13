package scheduledruntime

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/rpcx"
)

// Service is the gRPC ScheduledRuntimeService implementation. It converts
// domain errors to the stable status contract and exposes no credential.
type Service struct {
	scheduledruntimev1.UnimplementedScheduledRuntimeServiceServer
	rt *Runtime
}

// NewService wires the gRPC surface.
func NewService(rt *Runtime) *Service { return &Service{rt: rt} }

// SubmitScheduledRun implements the idempotent acceptance RPC.
func (s *Service) SubmitScheduledRun(ctx context.Context, req *scheduledruntimev1.SubmitScheduledRunRequest) (*scheduledruntimev1.SubmitScheduledRunResponse, error) {
	res, err := s.rt.Submit(ctx, SubmitInput{
		OccurrenceID:      req.GetOccurrenceId(),
		TaskID:            req.GetTaskId(),
		RunID:             req.GetRunId(),
		SessionID:         req.GetSessionId(),
		Attempt:           int(req.GetAttempt()),
		ConfigFingerprint: req.GetConfigFingerprint(),
	})
	if err != nil {
		return nil, rpcx.ToStatus(err)
	}
	rec := res.Receipt
	return &scheduledruntimev1.SubmitScheduledRunResponse{
		OccurrenceId:      rec.OccurrenceID,
		TaskId:            rec.TaskID,
		RunId:             rec.RunID,
		SessionId:         rec.SessionID,
		ReceiptGeneration: rec.ReceiptGeneration,
		Status:            rec.Status,
		AcceptedAt:        timestamppb.New(rec.AcceptedAt),
		AlreadyAccepted:   res.AlreadyAccepted,
	}, nil
}

// GetScheduledExecution returns the authoritative execution fact.
func (s *Service) GetScheduledExecution(ctx context.Context, req *scheduledruntimev1.GetScheduledExecutionRequest) (*scheduledruntimev1.GetScheduledExecutionResponse, error) {
	rec, err := s.rt.GetExecution(ctx, req.GetOccurrenceId())
	if err != nil {
		return nil, rpcx.ToStatus(err)
	}
	return &scheduledruntimev1.GetScheduledExecutionResponse{Execution: executionProto(rec)}, nil
}

// CancelScheduledRun requests cancellation (not a stop proof).
func (s *Service) CancelScheduledRun(ctx context.Context, req *scheduledruntimev1.CancelScheduledRunRequest) (*scheduledruntimev1.CancelScheduledRunResponse, error) {
	accepted, status, err := s.rt.CancelExecution(ctx, req.GetOccurrenceId(), req.GetReason())
	if err != nil {
		return nil, rpcx.ToStatus(err)
	}
	return &scheduledruntimev1.CancelScheduledRunResponse{Accepted: accepted, Status: status}, nil
}

func executionProto(rec *model.ScheduledExecutionReceipt) *scheduledruntimev1.ScheduledExecution {
	out := &scheduledruntimev1.ScheduledExecution{
		OccurrenceId:      rec.OccurrenceID,
		TaskId:            rec.TaskID,
		UserId:            rec.UserID,
		RunId:             rec.RunID,
		SessionId:         rec.SessionID,
		ReceiptGeneration: rec.ReceiptGeneration,
		Status:            rec.Status,
		Reason:            rec.Reason,
		LastError:         rec.LastError,
		ConfigFingerprint: rec.ConfigFingerprint,
		GrantGeneration:   rec.GrantGeneration,
		Terminal:          rec.Terminal(),
		Started:           rec.StartIntentAt != nil,
		Uncertain:         rec.Status == model.ScheduledReceiptUncertain && !rec.StopProofPresent,
		AcceptedAt:        ts(rec.AcceptedAt),
		StartIntentAt:     tsPtr(rec.StartIntentAt),
		StartedAt:         tsPtr(rec.StartedAt),
		FinishedAt:        tsPtr(rec.FinishedAt),
		GrantDeadline:     tsPtr(rec.GrantDeadline),
	}
	if rec.StopProofPresent {
		out.StopProof = &scheduledruntimev1.ExecutionStopProof{
			Present:      true,
			OccurrenceId: rec.OccurrenceID,
			RunId:        rec.RunID,
			Generation:   rec.StopProofGen,
			ProvenAt:     tsPtr(rec.StopProofAt),
			Kind:         rec.StopProofKind,
		}
	}
	return out
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}
