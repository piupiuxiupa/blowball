package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/store/mysql"
)

// fakeScheduledStore is an in-memory ScheduledSessionStore. Source is
// configurable so tests can prove the constructor rejects the interactive
// group.
type fakeScheduledStore struct {
	source   model.SessionSource
	sessions map[string]model.Session
	titles   map[string]model.Title
	messages map[string][]model.Message
	calls    map[string][]model.ToolCall
	deleted  []string
}

func newFakeScheduledStore() *fakeScheduledStore {
	return &fakeScheduledStore{
		source:   model.SessionSourceScheduled,
		sessions: map[string]model.Session{},
		titles:   map[string]model.Title{},
		messages: map[string][]model.Message{},
		calls:    map[string][]model.ToolCall{},
	}
}

func (f *fakeScheduledStore) Source() model.SessionSource { return f.source }

func (f *fakeScheduledStore) CreateScheduledSession(_ context.Context, sess model.Session) (*model.Session, error) {
	if existing, ok := f.sessions[sess.SessionID]; ok {
		if existing.OccurrenceID != sess.OccurrenceID {
			return nil, mysql.ErrScheduledSessionConflict
		}
		return &existing, nil
	}
	sess.Source = model.SessionSourceScheduled
	f.sessions[sess.SessionID] = sess
	return &sess, nil
}

func (f *fakeScheduledStore) GetScheduledSessionByID(_ context.Context, sessionID string) (*model.Session, error) {
	sess, ok := f.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	sess.Source = model.SessionSourceScheduled
	return &sess, nil
}

func (f *fakeScheduledStore) ListScheduledSessions(_ context.Context, userID, taskID string, offset, limit int) ([]mysql.SessionWithTitle, error) {
	out := []mysql.SessionWithTitle{}
	for _, sess := range f.sessions {
		if sess.UserID != userID {
			continue
		}
		if taskID != "" && sess.TaskID != taskID {
			continue
		}
		out = append(out, mysql.SessionWithTitle{
			SessionID: sess.SessionID, UserID: sess.UserID, TaskID: sess.TaskID,
			OccurrenceID: sess.OccurrenceID, RunID: sess.RunID, Title: f.titles[sess.SessionID].Title,
		})
	}
	return out, nil
}

func (f *fakeScheduledStore) DeleteSession(_ context.Context, sessionID string) error {
	f.deleted = append(f.deleted, sessionID)
	delete(f.sessions, sessionID)
	return nil
}

func (f *fakeScheduledStore) ListMessagesPaged(_ context.Context, sessionID, _ string, _ int, _, _ string) ([]model.Message, string, error) {
	return f.messages[sessionID], "", nil
}

func (f *fakeScheduledStore) ListToolCalls(_ context.Context, sessionID string) ([]model.ToolCall, error) {
	return f.calls[sessionID], nil
}

func (f *fakeScheduledStore) GetTitle(_ context.Context, sessionID string) (*model.Title, error) {
	t, ok := f.titles[sessionID]
	if !ok {
		return nil, nil
	}
	return &t, nil
}

func newScheduledFixture(t *testing.T) (*fakeScheduledStore, *ScheduledSessions, *fakeRedisStore) {
	t.Helper()
	store := newFakeScheduledStore()
	redis := &fakeRedisStore{}
	return store, NewScheduledSessions(store, redis), redis
}

func TestScheduledSessions_CrossUserReadsAreNotFound(t *testing.T) {
	store, svc, _ := newScheduledFixture(t)
	ctx := context.Background()
	store.sessions["ss-1"] = model.Session{SessionID: "ss-1", UserID: "owner", TaskID: "task-1"}

	got, err := svc.Get(ctx, "intruder", "ss-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	_, _, err = svc.Messages(ctx, "intruder", "ss-1", "", 20, "asc", model.SubagentContentFull)
	assert.ErrorIs(t, err, ErrScheduledSessionNotFound)

	_, err = svc.ToolCalls(ctx, "intruder", "ss-1")
	assert.ErrorIs(t, err, ErrScheduledSessionNotFound)

	err = svc.Delete(ctx, "intruder", "ss-1")
	assert.ErrorIs(t, err, ErrScheduledSessionNotFound)
	assert.Empty(t, store.deleted)
}

func TestScheduledSessions_DeleteRejectsActiveExecution(t *testing.T) {
	store, svc, redis := newScheduledFixture(t)
	ctx := context.Background()
	store.sessions["ss-2"] = model.Session{SessionID: "ss-2", UserID: "owner", TaskID: "task-1"}
	svc.WithDeletionGuard(func(context.Context, string) (string, error) {
		return model.ScheduledReasonResultUnknown, nil
	})

	err := svc.Delete(ctx, "owner", "ss-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrScheduledSessionActive)
	assert.Empty(t, store.deleted, "an active session must not be archived")
	assert.Zero(t, redis.clearCalls)
}

func TestScheduledSessions_DeleteArchivesAndClearsCache(t *testing.T) {
	store, svc, redis := newScheduledFixture(t)
	ctx := context.Background()
	store.sessions["ss-3"] = model.Session{SessionID: "ss-3", UserID: "owner", TaskID: "task-1"}
	deletedHook := 0
	svc.WithDeletionGuard(func(context.Context, string) (string, error) { return "", nil }).
		WithDeletedHook(func(context.Context, string) { deletedHook++ })

	require.NoError(t, svc.Delete(ctx, "owner", "ss-3"))
	assert.Equal(t, []string{"ss-3"}, store.deleted)
	assert.Equal(t, 1, redis.clearCalls)
	assert.Equal(t, 1, deletedHook)
}

func TestScheduledSessions_ListMapsSourceAndTaskFilter(t *testing.T) {
	store, svc, _ := newScheduledFixture(t)
	ctx := context.Background()
	store.sessions["ss-4"] = model.Session{SessionID: "ss-4", UserID: "owner", TaskID: "task-1", OccurrenceID: "occ-1", RunID: "run-1"}
	store.sessions["ss-5"] = model.Session{SessionID: "ss-5", UserID: "owner", TaskID: "task-2", OccurrenceID: "occ-2", RunID: "run-2"}
	store.sessions["ss-6"] = model.Session{SessionID: "ss-6", UserID: "other", TaskID: "task-1"}

	rows, err := svc.List(ctx, "owner", "task-1", 0, 20)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, model.SessionSourceScheduled, rows[0].Source)
	assert.Equal(t, "occ-1", rows[0].OccurrenceID)
}

func TestNewScheduledSessions_RejectsInteractiveStore(t *testing.T) {
	store := newFakeScheduledStore()
	store.source = model.SessionSourceInteractive
	assert.Panics(t, func() { NewScheduledSessions(store, nil) })
}
