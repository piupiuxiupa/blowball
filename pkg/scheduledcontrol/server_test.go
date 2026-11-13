package scheduledcontrol

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/modelselect"
	"github.com/lush/blowball/pkg/resource"
)

type fakeUsers struct {
	user *model.User
	err  error
}

func (f *fakeUsers) GetUserByID(context.Context, string) (*model.User, error) {
	return f.user, f.err
}

type fakeResources struct {
	bindingErr error
	draft      *resource.DraftInfo
	version    *resource.VersionInfo
	published  *model.ScheduledResourceVersion
}

func (f *fakeResources) GetDraft(context.Context, string, string) (*resource.DraftInfo, error) {
	return f.draft, nil
}

func (f *fakeResources) Publish(context.Context, string, string, uint64, string) (*model.ScheduledResourceVersion, error) {
	return f.published, nil
}

func (f *fakeResources) GetVersion(context.Context, string, string, uint64) (*resource.VersionInfo, error) {
	return f.version, nil
}

func (f *fakeResources) ValidateBinding(context.Context, string, string, uint64, string) error {
	return f.bindingErr
}

func controlFixture(t *testing.T) (*Service, *fakeUsers, *fakeResources) {
	t.Helper()
	users := &fakeUsers{user: &model.User{UserID: "user-1", Status: model.UserStatusActive}}
	res := &fakeResources{}
	svc := New(users, res, modelselect.Config{
		Catalog: []config.ModelCatalogEntry{{Name: "model-a", MaxContextTokens: 100000, MaxCompletionTokens: 8000}},
		Default: "model-a", DefaultEffort: "none",
	})
	return svc, users, res
}

func TestValidateTaskActorAndConfig(t *testing.T) {
	svc, users, res := controlFixture(t)
	ctx := context.Background()

	resp, err := svc.ValidateTaskActorAndConfig(ctx, &schedulerv1.ValidateTaskActorAndConfigRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
		Config: &schedulerv1.TaskConfig{Name: "n", Prompt: "p", Model: "model-a", ReasoningEffort: "none",
			ResourceRevision: 2, ManifestHash: "hash"},
	})
	require.NoError(t, err)
	assert.Equal(t, "model-a", resp.GetResolvedModel())
	assert.Equal(t, "none", resp.GetResolvedReasoningEffort())

	users.user = &model.User{UserID: "user-1", Status: model.UserStatusDisabled}
	_, err = svc.ValidateTaskActorAndConfig(ctx, &schedulerv1.ValidateTaskActorAndConfigRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1", Config: &schedulerv1.TaskConfig{Model: "model-a"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	users.user = &model.User{UserID: "user-1", Status: model.UserStatusActive}
	_, err = svc.ValidateTaskActorAndConfig(ctx, &schedulerv1.ValidateTaskActorAndConfigRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1", Config: &schedulerv1.TaskConfig{Model: "nope"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	res.bindingErr = resource.ErrNotFound
	_, err = svc.ValidateTaskActorAndConfig(ctx, &schedulerv1.ValidateTaskActorAndConfigRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
		Config: &schedulerv1.TaskConfig{Model: "model-a", ResourceRevision: 2, ManifestHash: "hash"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestControlRequiresActor(t *testing.T) {
	svc, _, _ := controlFixture(t)
	_, err := svc.GetResourceDraft(context.Background(), &schedulerv1.GetResourceDraftRequest{TaskId: "t"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestResourceConversions(t *testing.T) {
	svc, _, res := controlFixture(t)
	ctx := context.Background()
	res.draft = &resource.DraftInfo{TaskID: "task-1", DraftRevision: 4, Files: []resource.FileEntry{{Path: "a.txt", Size: 3, SHA256: "abc"}}}
	draft, err := svc.GetResourceDraft(ctx, &schedulerv1.GetResourceDraftRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(4), draft.GetDraftRevision())
	assert.Equal(t, "a.txt", draft.GetFiles()[0].GetPath())
	assert.Equal(t, ".cron/task-1/draft", draft.GetDraftRoot())

	res.version = &resource.VersionInfo{TaskID: "task-1", Revision: 2, ManifestHash: "h", FileCount: 1, TotalBytes: 3,
		Files: []resource.FileEntry{{Path: "a.txt", Size: 3, SHA256: "abc"}}}
	v, err := svc.GetResourceVersion(ctx, &schedulerv1.GetResourceVersionRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1", ResourceRevision: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), v.GetVersion().GetResourceRevision())
	assert.Equal(t, "a.txt", v.GetVersion().GetFiles()[0].GetPath())

	res.published = &model.ScheduledResourceVersion{TaskID: "task-1", Revision: 3, ManifestHash: "h3", FileCount: 2, TotalBytes: 9}
	p, err := svc.PublishResource(ctx, &schedulerv1.PublishResourceRequest{
		Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1", ExpectedDraftRevision: 4, RequestId: "req",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(3), p.GetResourceRevision())
	assert.Equal(t, uint32(2), p.GetFileCount())
}
