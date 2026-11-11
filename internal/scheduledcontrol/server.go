// Package scheduledcontrol implements the main service's
// SchedulerControlService: owner/model/resource validation and the resource
// metadata/publish RPC surface used by the Scheduler and the agent tools. It
// never calls the LLM or runs an agent.
package scheduledcontrol

import (
	"context"
	"errors"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/resource"
	"github.com/lush/blowball/internal/rpcx"
	"go.uber.org/zap"
)

// UserLookup is the active-owner check.
type UserLookup interface {
	GetUserByID(ctx context.Context, userID string) (*model.User, error)
}

// ResourceService is the `.cron` service surface (implemented by
// *resource.Service).
type ResourceService interface {
	GetDraft(ctx context.Context, userID, taskID string) (*resource.DraftInfo, error)
	Publish(ctx context.Context, userID, taskID string, expectedDraftRevision uint64, requestID string) (*model.ScheduledResourceVersion, error)
	GetVersion(ctx context.Context, userID, taskID string, revision uint64) (*resource.VersionInfo, error)
	ValidateBinding(ctx context.Context, userID, taskID string, revision uint64, manifestHash string) error
}

// Service implements schedulerv1.SchedulerControlServiceServer.
type Service struct {
	schedulerv1.UnimplementedSchedulerControlServiceServer
	users     UserLookup
	resources ResourceService
	selection modelselect.Config
	limits    resource.Limits
}

// New wires the control service.
func New(users UserLookup, resources ResourceService, selection modelselect.Config) *Service {
	return &Service{users: users, resources: resources, selection: selection}
}

// ValidateTaskActorAndConfig re-validates the owner, model/effort and resource
// binding. It returns the actual resolved model/effort.
func (s *Service) ValidateTaskActorAndConfig(ctx context.Context, req *schedulerv1.ValidateTaskActorAndConfigRequest) (*schedulerv1.ValidateTaskActorAndConfigResponse, error) {
	actor, err := s.requireActiveActor(ctx, req.GetActor())
	if err != nil {
		return nil, err
	}
	cfg := req.GetConfig()
	sel, code, message := modelselect.Resolve(s.selection, cfg.GetModel(), cfg.GetReasoningEffort())
	if code != "" {
		return nil, rpcx.ToStatus(rpcx.Errorf(code, "%s", message))
	}
	if cfg.GetResourceRevision() > 0 || cfg.GetManifestHash() != "" {
		if req.GetTaskId() == "" {
			return nil, rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "resource binding requires an existing task"))
		}
		if err := s.resources.ValidateBinding(ctx, actor, req.GetTaskId(), cfg.GetResourceRevision(), cfg.GetManifestHash()); err != nil {
			return nil, resourceErr(err)
		}
	}
	return &schedulerv1.ValidateTaskActorAndConfigResponse{
		ResolvedModel:           sel.Model,
		ResolvedReasoningEffort: sel.Effort,
	}, nil
}

// ValidateResourceBinding verifies a published version belongs to the actor's
// task and the manifest hash matches.
func (s *Service) ValidateResourceBinding(ctx context.Context, req *schedulerv1.ValidateResourceBindingRequest) (*schedulerv1.ValidateResourceBindingResponse, error) {
	actor, err := s.requireActiveActor(ctx, req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := s.resources.ValidateBinding(ctx, actor, req.GetTaskId(), req.GetResourceRevision(), req.GetManifestHash()); err != nil {
		return nil, resourceErr(err)
	}
	return &schedulerv1.ValidateResourceBindingResponse{}, nil
}

// GetResourceVersion returns immutable publication metadata.
func (s *Service) GetResourceVersion(ctx context.Context, req *schedulerv1.GetResourceVersionRequest) (*schedulerv1.GetResourceVersionResponse, error) {
	actor, err := s.requireActiveActor(ctx, req.GetActor())
	if err != nil {
		return nil, err
	}
	v, err := s.resources.GetVersion(ctx, actor, req.GetTaskId(), req.GetResourceRevision())
	if err != nil {
		return nil, resourceErr(err)
	}
	return &schedulerv1.GetResourceVersionResponse{Version: versionProto(v)}, nil
}

// GetResourceDraft returns the editable draft revision and file list.
func (s *Service) GetResourceDraft(ctx context.Context, req *schedulerv1.GetResourceDraftRequest) (*schedulerv1.GetResourceDraftResponse, error) {
	actor, err := s.requireActiveActor(ctx, req.GetActor())
	if err != nil {
		return nil, err
	}
	d, err := s.resources.GetDraft(ctx, actor, req.GetTaskId())
	if err != nil {
		return nil, resourceErr(err)
	}
	files := make([]*schedulerv1.ResourceFile, 0, len(d.Files))
	for _, f := range d.Files {
		files = append(files, &schedulerv1.ResourceFile{Path: f.Path, Size: uint64(f.Size), Sha256: f.SHA256})
	}
	return &schedulerv1.GetResourceDraftResponse{
		DraftRevision: d.DraftRevision,
		Files:         files,
		DraftRoot:     resource.CronDirName + "/" + req.GetTaskId() + "/" + resource.DraftDir,
	}, nil
}

// PublishResource materializes the draft into an immutable revision.
func (s *Service) PublishResource(ctx context.Context, req *schedulerv1.PublishResourceRequest) (*schedulerv1.PublishResourceResponse, error) {
	actor, err := s.requireActiveActor(ctx, req.GetActor())
	if err != nil {
		return nil, err
	}
	v, err := s.resources.Publish(ctx, actor, req.GetTaskId(), req.GetExpectedDraftRevision(), req.GetRequestId())
	if err != nil {
		logger.FromContext(ctx).Warn("cron resource publish failed",
			zap.String("op", "resource.publish"),
			zap.String("task_id", req.GetTaskId()),
			zap.String("request_id", req.GetRequestId()),
			zap.Error(err))
		return nil, resourceErr(err)
	}
	logger.FromContext(ctx).Info("cron resource published",
		zap.String("op", "resource.publish"),
		zap.String("task_id", v.TaskID),
		zap.Uint64("revision", v.Revision),
		zap.String("manifest_hash", v.ManifestHash),
		zap.Int("file_count", v.FileCount),
		zap.Int64("total_bytes", v.TotalBytes))
	return &schedulerv1.PublishResourceResponse{
		ResourceRevision: v.Revision,
		ManifestHash:     v.ManifestHash,
		FileCount:        uint32(v.FileCount),
		TotalBytes:       uint64(v.TotalBytes),
	}, nil
}

func (s *Service) requireActiveActor(ctx context.Context, actor *schedulerv1.Actor) (string, error) {
	if actor == nil || actor.GetUserId() == "" {
		return "", rpcx.ToStatus(rpcx.Errorf(rpcx.CodeUnauthenticated, "actor is required"))
	}
	user, err := s.users.GetUserByID(ctx, actor.GetUserId())
	if err != nil {
		return "", rpcx.ToStatus(rpcx.Wrap(rpcx.CodeUnavailable, "owner lookup failed", err))
	}
	if user == nil || user.Status != model.UserStatusActive {
		return "", rpcx.ToStatus(rpcx.Errorf(rpcx.CodeUserUnavailable, "actor is not active"))
	}
	return user.UserID, nil
}

func versionProto(v *resource.VersionInfo) *schedulerv1.ResourceVersion {
	out := &schedulerv1.ResourceVersion{
		TaskId: v.TaskID, ResourceRevision: v.Revision, ManifestHash: v.ManifestHash,
		FileCount: uint32(v.FileCount), TotalBytes: uint64(v.TotalBytes),
	}
	for _, f := range v.Files {
		out.Files = append(out.Files, &schedulerv1.ResourceFile{Path: f.Path, Size: uint64(f.Size), Sha256: f.SHA256})
	}
	return out
}

func resourceErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, resource.ErrNotFound):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeNotFound, "cron resource not found"))
	case errors.Is(err, resource.ErrNamespaceConflict):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeResourceConflict, "%s", err.Error()))
	case errors.Is(err, resource.ErrReadOnly):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeResourceReadOnly, "%s", err.Error()))
	case errors.Is(err, resource.ErrDraftConflict):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeRevisionConflict, "%s", err.Error()))
	case errors.Is(err, resource.ErrLimit):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeResourceLimit, "%s", err.Error()))
	case errors.Is(err, resource.ErrInvalidPath), errors.Is(err, resource.ErrInvalidRequest):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeInvalidArgument, "%s", err.Error()))
	case errors.Is(err, resource.ErrUnavailable):
		return rpcx.ToStatus(rpcx.Errorf(rpcx.CodeUnavailable, "%s", err.Error()))
	default:
		return rpcx.ToStatus(rpcx.Wrap(rpcx.CodeUnavailable, "cron resource operation failed", err))
	}
}
