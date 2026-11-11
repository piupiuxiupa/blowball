package turn

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/service"
)

func TestForSource_InteractiveAndLegacyUseTheDefaultView(t *testing.T) {
	e := &Executor{}
	got, err := e.forSource(model.SessionSourceInteractive)
	require.NoError(t, err)
	assert.Same(t, e, got)

	got, err = e.forSource("")
	require.NoError(t, err)
	assert.Same(t, e, got, "an unset source keeps the interactive default")

	got, err = e.forSource(model.SessionSourceLegacyInteractive)
	require.NoError(t, err)
	assert.Same(t, e, got)
}

func TestForSource_ScheduledFailsClosedWithoutServices(t *testing.T) {
	e := &Executor{}
	_, err := e.forSource(model.SessionSourceScheduled)
	require.Error(t, err)

	// Start must reject the scheduled request before any preamble read or
	// write — no silent interactive fallback.
	_, err = e.Start(context.Background(), &Request{
		Source: model.SessionSourceScheduled, SessionID: "s", UserID: "u", RunID: "r",
	})
	require.Error(t, err)
}

func TestForSource_ScheduledSwapsEveryPersistenceService(t *testing.T) {
	deps := service.SessionDeps{}
	schedSessions := service.NewSessionService(deps)
	schedMessages := service.NewMessageService(deps, nil)
	schedCompaction := service.NewCompactionService(deps, nil, "", 0)
	interactiveSessions := service.NewSessionService(deps)
	snapshots := &fakeSnapshotStore{}

	e := &Executor{
		sessSvc: interactiveSessions,
		scheduled: &ScheduledServices{
			Sessions: schedSessions, Messages: schedMessages, Compaction: schedCompaction,
			Snapshots: snapshots,
		},
	}

	clone, err := e.forSource(model.SessionSourceScheduled)
	require.NoError(t, err)
	assert.NotSame(t, e, clone, "the scheduled path must not mutate the shared executor")
	assert.Same(t, schedSessions, clone.sessSvc)
	assert.Same(t, schedMessages, clone.msgSvc)
	assert.Same(t, schedCompaction, clone.compSvc)
	assert.Same(t, snapshots, clone.snapshots)
	assert.Same(t, interactiveSessions, e.sessSvc, "the original executor is unchanged")
}

type fakeSnapshotStore struct{}

func (f *fakeSnapshotStore) AppendSubAgentRun(context.Context, model.SubAgentInstance, model.SubAgentRun) error {
	return nil
}

func TestForSource_UnknownSourceRejected(t *testing.T) {
	e := &Executor{}
	_, err := e.forSource(model.SessionSource("client_supplied_table"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown session source")
}
