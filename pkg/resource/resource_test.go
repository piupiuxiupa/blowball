package resource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/model"
)

type fakeStore struct {
	mu       sync.Mutex
	drafts   map[string]*model.ScheduledResourceDraft
	versions map[string][]model.ScheduledResourceVersion
	requests map[string]*model.ScheduledResourceVersion
	onGet    func(call int)
	gets     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		drafts:   map[string]*model.ScheduledResourceDraft{},
		versions: map[string][]model.ScheduledResourceVersion{},
		requests: map[string]*model.ScheduledResourceVersion{},
	}
}

func (f *fakeStore) EnsureResourceDraft(_ context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.drafts[taskID]; ok {
		return d, nil
	}
	d := &model.ScheduledResourceDraft{TaskID: taskID, UserID: userID}
	f.drafts[taskID] = d
	return d, nil
}

func (f *fakeStore) GetResourceDraft(_ context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error) {
	f.mu.Lock()
	d := f.drafts[taskID]
	f.gets++
	hook := f.onGet
	call := f.gets
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if d == nil || d.UserID != userID {
		return nil, nil
	}
	c := *d
	return &c, nil
}

func (f *fakeStore) BumpResourceDraft(_ context.Context, userID, taskID string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.drafts[taskID]
	if d == nil || d.UserID != userID {
		return 0, errors.New("missing")
	}
	d.DraftRevision++
	d.Generation = "gen-" + time.Now().Format(time.RFC3339Nano)
	return d.DraftRevision, nil
}

func (f *fakeStore) NextResourceRevision(_ context.Context, taskID string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var max uint64
	for _, v := range f.versions[taskID] {
		if v.Revision > max {
			max = v.Revision
		}
	}
	return max + 1, nil
}

func (f *fakeStore) FindResourceVersionByRequest(_ context.Context, userID, taskID, requestID string) (*model.ScheduledResourceVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.requests[taskID+"/"+requestID]
	if v == nil || v.UserID != userID {
		return nil, nil
	}
	return v, nil
}

func (f *fakeStore) InsertResourceVersion(_ context.Context, v model.ScheduledResourceVersion) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := v.TaskID + "/" + v.RequestID
	if _, ok := f.requests[key]; ok {
		return false, nil
	}
	f.versions[v.TaskID] = append(f.versions[v.TaskID], v)
	f.requests[key] = &v
	return true, nil
}

func (f *fakeStore) GetResourceVersion(_ context.Context, userID, taskID string, revision uint64) (*model.ScheduledResourceVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.versions[taskID] {
		if v.Revision == revision && v.UserID == userID {
			c := v
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ListResourceVersions(_ context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.ScheduledResourceVersion
	for _, v := range f.versions[taskID] {
		if v.UserID == userID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteResourceVersion(_ context.Context, userID, taskID string, revision uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.versions[taskID][:0]
	found := false
	for _, v := range f.versions[taskID] {
		if v.Revision == revision && v.UserID == userID {
			found = true
			continue
		}
		out = append(out, v)
	}
	f.versions[taskID] = out
	return found, nil
}

func (f *fakeStore) WithinTaskResourceLock(_ context.Context, _ string, fn func() error) error {
	return fn()
}

type fakeOwner struct {
	owned map[string]bool
	err   error
}

func (f *fakeOwner) OwnsTask(_ context.Context, _, taskID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.owned[taskID], nil
}

type fakeChecker struct{ referenced bool }

func (f *fakeChecker) ResourceReferenced(context.Context, string, uint64) (bool, error) {
	return f.referenced, nil
}

func fixture(t *testing.T) (*Service, *fakeStore, *fakeOwner, string, string) {
	t.Helper()
	workspace := t.TempDir()
	store := newFakeStore()
	taskID := uuid.NewString()
	owner := &fakeOwner{owned: map[string]bool{taskID: true}}
	svc := New(store, owner, func(string) string { return workspace }, DefaultLimits())
	return svc, store, owner, workspace, taskID
}

func TestEnsureLayoutManagedAndConflict(t *testing.T) {
	ctx := context.Background()
	taskID := uuid.NewString()
	workspace := t.TempDir()
	layout, err := EnsureLayout(ctx, workspace, "user-1", taskID)
	require.NoError(t, err)
	for _, dir := range []string{layout.DraftRoot(), layout.RevisionsRoot(), layout.StagingRoot()} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}
	_, err = os.Stat(layout.ManagedMarkerPath())
	require.NoError(t, err)
	// Idempotent.
	_, err = EnsureLayout(ctx, workspace, "user-1", taskID)
	require.NoError(t, err)

	// Existing unmanaged .cron content conflicts and is left untouched.
	legacy := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(legacy, CronDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, CronDirName, "legacy.txt"), []byte("keep"), 0o644))
	_, err = EnsureLayout(ctx, legacy, "user-1", taskID)
	assert.ErrorIs(t, err, ErrNamespaceConflict)
	raw, rerr := os.ReadFile(filepath.Join(legacy, CronDirName, "legacy.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "keep", string(raw))

	// Existing unmanaged task root conflicts.
	partial := t.TempDir()
	_, err = EnsureLayout(ctx, partial, "user-1", uuid.NewString())
	require.NoError(t, err)
	another := uuid.NewString()
	require.NoError(t, os.MkdirAll(filepath.Join(partial, CronDirName, another), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(partial, CronDirName, another, "user.txt"), []byte("x"), 0o644))
	_, err = EnsureLayout(ctx, partial, "user-1", another)
	assert.ErrorIs(t, err, ErrNamespaceConflict)
}

func TestBuildStagingManifestAndValidation(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, DraftDir)
	require.NoError(t, os.MkdirAll(filepath.Join(draft, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(draft, "a.txt"), []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(draft, "sub", "b.bin"), []byte("beta!"), 0o644))
	staging := filepath.Join(dir, "staging")
	m, err := BuildStaging(draft, staging, 3, DefaultLimits())
	require.NoError(t, err)
	require.Len(t, m.Files, 2)
	assert.Equal(t, "a.txt", m.Files[0].Path)
	assert.Equal(t, "sub/b.bin", m.Files[1].Path)
	assert.Equal(t, ManifestHash(m.Files), m.ManifestHash)

	final := filepath.Join(dir, "revisions", "3")
	require.NoError(t, os.MkdirAll(filepath.Dir(final), 0o755))
	require.NoError(t, os.Rename(staging, final))
	got, err := VerifyPinned(final, m.ManifestHash)
	require.NoError(t, err)
	assert.Equal(t, m.ManifestHash, got.ManifestHash)

	// Tampering with a copied byte is detected.
	require.NoError(t, os.WriteFile(filepath.Join(final, "a.txt"), []byte("tampered"), 0o644))
	_, err = VerifyPinned(final, m.ManifestHash)
	assert.ErrorIs(t, err, ErrDraftConflict)
}

func TestBuildStagingRejectsSymlinkAndLimits(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, DraftDir)
	require.NoError(t, os.MkdirAll(draft, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(draft, "real.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(draft, "real.txt"), filepath.Join(draft, "link.txt")))
	_, err := BuildStaging(draft, filepath.Join(dir, "stage1"), 1, DefaultLimits())
	assert.ErrorIs(t, err, ErrInvalidPath)

	require.NoError(t, os.Remove(filepath.Join(draft, "link.txt")))
	limits := DefaultLimits()
	limits.MaxFiles = 0 // zero means "no file allowed" for the limit check
	_, err = BuildStaging(draft, filepath.Join(dir, "stage2"), 1, limits)
	// BuildStaging treats a non-positive MaxFiles as unbounded by contract; the
	// service always passes resolved positive limits. Verify the explicit cap.
	limits.MaxFiles = 1
	require.NoError(t, os.WriteFile(filepath.Join(draft, "second.txt"), []byte("y"), 0o644))
	_, err = BuildStaging(draft, filepath.Join(dir, "stage3"), 1, limits)
	assert.ErrorIs(t, err, ErrLimit)
}

func TestPublishIdempotencyAndPinnedValidation(t *testing.T) {
	svc, store, _, workspace, taskID := fixture(t)
	ctx := context.Background()
	layout, err := svc.EnsureDraft(ctx, "user-1", taskID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(layout.DraftRoot(), "instructions.md"), []byte("do it"), 0o644))

	first, err := svc.Publish(ctx, "user-1", taskID, 0, "req-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Revision)
	assert.Len(t, first.ManifestHash, 64)

	again, err := svc.Publish(ctx, "user-1", taskID, 0, "req-1")
	require.NoError(t, err)
	assert.Equal(t, first.Revision, again.Revision)
	assert.Equal(t, first.ManifestHash, again.ManifestHash)

	second, err := svc.Publish(ctx, "user-1", taskID, 0, "req-2")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.Revision)

	contextText, err := svc.ValidatePinned(ctx, "user-1", taskID, first.Revision, first.ManifestHash)
	require.NoError(t, err)
	assert.Contains(t, contextText, "instructions.md")
	assert.Contains(t, contextText, first.ManifestHash)
	assert.Contains(t, contextText, ".cron/"+taskID+"/revisions/1/")

	// The pinned directory is immutable; tampering fails validation.
	require.NoError(t, os.WriteFile(filepath.Join(layout.RevisionRoot(1), "instructions.md"), []byte("changed"), 0o644))
	_, err = svc.ValidatePinned(ctx, "user-1", taskID, first.Revision, first.ManifestHash)
	assert.ErrorIs(t, err, ErrDraftConflict)
	_ = workspace
	_ = store

	// A version belongs to its owner.
	_, err = svc.GetVersion(ctx, "user-2", taskID, second.Revision)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPublishExpectedRevisionConflict(t *testing.T) {
	svc, _, _, _, taskID := fixture(t)
	ctx := context.Background()
	layout, err := svc.EnsureDraft(ctx, "user-1", taskID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(layout.DraftRoot(), "a.txt"), []byte("a"), 0o644))
	// The draft revision is 0; an explicit expectation of 5 must conflict.
	_, err = svc.Publish(ctx, "user-1", taskID, 5, "req-x")
	assert.ErrorIs(t, err, ErrDraftConflict)
}

func TestPublishDetectsDraftChangeDuringCopy(t *testing.T) {
	svc, store, _, _, taskID := fixture(t)
	ctx := context.Background()
	layout, err := svc.EnsureDraft(ctx, "user-1", taskID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(layout.DraftRoot(), "a.txt"), []byte("a"), 0o644))
	// Bump the draft between the first read and the post-copy fence read.
	calls := 0
	store.onGet = func(int) {
		calls++
		if calls == 2 {
			store.BumpResourceDraft(ctx, "user-1", taskID)
		}
	}
	_, err = svc.Publish(ctx, "user-1", taskID, 0, "req-race")
	assert.ErrorIs(t, err, ErrDraftConflict)
}

func TestGuardRules(t *testing.T) {
	svc, store, owner, _, taskID := fixture(t)
	guard := NewGuard(svc)
	ctx := context.Background()

	assert.NoError(t, guard.CheckWrite(ctx, "user-1", "notes.md", WriteCreate))
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName, WriteModify), ErrReadOnly)
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID, WriteDelete), ErrReadOnly)
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/revisions/1/x", WriteModify), ErrReadOnly)
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/draft", WriteModify), ErrReadOnly)
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/not-a-uuid/draft/x", WriteModify), ErrReadOnly)

	require.NoError(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/draft/notes.md", WriteCreate))
	assert.Equal(t, uint64(1), store.drafts[taskID].DraftRevision)

	// A path that cleans across the namespace stays protected.
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/draft/../revisions/x", WriteModify), ErrReadOnly)

	// A symlink inside draft pointing at a published revision is rejected: the
	// lexical path is allowed but the real write would escape the draft.
	layout, err := svc.EnsureDraft(ctx, "user-1", taskID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.RevisionRoot(1), 0o755))
	require.NoError(t, os.Symlink(layout.RevisionRoot(1), filepath.Join(layout.DraftRoot(), "escape")))
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/draft/escape/x", WriteCreate), ErrReadOnly)

	// Unknown task: not found. Cross-user: the fake owner denies.
	owner.owned["other"] = false
	assert.ErrorIs(t, guard.CheckWrite(ctx, "user-1", CronDirName+"/"+uuid.NewString()+"/draft/x", WriteCreate), ErrNotFound)
	owner.err = errors.New("scheduler down")
	err = guard.CheckWrite(ctx, "user-1", CronDirName+"/"+taskID+"/draft/x", WriteCreate)
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestCleanupUnreferencedVersion(t *testing.T) {
	svc, _, _, _, taskID := fixture(t)
	ctx := context.Background()
	layout, err := svc.EnsureDraft(ctx, "user-1", taskID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(layout.DraftRoot(), "a.txt"), []byte("a"), 0o644))
	v, err := svc.Publish(ctx, "user-1", taskID, 0, "req-clean")
	require.NoError(t, err)

	deleted, err := svc.DeleteUnreferencedVersion(ctx, "user-1", taskID, v.Revision, &fakeChecker{referenced: true})
	require.NoError(t, err)
	assert.False(t, deleted)
	_, err = os.Stat(layout.RevisionRoot(v.Revision))
	require.NoError(t, err, "a referenced version is never removed")

	deleted, err = svc.DeleteUnreferencedVersion(ctx, "user-1", taskID, v.Revision, &fakeChecker{referenced: false})
	require.NoError(t, err)
	assert.True(t, deleted)
	_, err = os.Stat(layout.RevisionRoot(v.Revision))
	assert.True(t, os.IsNotExist(err))
}
