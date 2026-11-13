package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/run"
	turnexec "github.com/lush/blowball/pkg/turn"
)

func backgroundRequest(t *testing.T, runID string) *turnexec.Request {
	t.Helper()
	sel, code, msg := resolveModelSelection(testSelectionConfig(), "", "")
	require.Empty(t, code, msg)
	now := time.Now().UTC()
	return &turnexec.Request{
		SessionID: "sess-1", UserID: "user-1", RunID: runID,
		Content: "offline work", Selection: sel.turnSelection(),
		UserMsgTime: now, MetaCreatedAt: now,
	}
}

func TestBackgroundSubmit_StableRunIDWithoutSubscriber(t *testing.T) {
	env := newPersistTestEnv(t, doneStub(), nil)
	req := backgroundRequest(t, "scheduled-run-1")
	submission := env.streamH.TurnExecutor().Submit(agent.WithUserID(context.Background(), "user-1"), req)
	require.Equal(t, turnexec.SubmitStarted, submission.Outcome, submission.Err)
	require.NotNil(t, submission.Started)

	select {
	case result := <-submission.Started.ResultCh:
		require.NoError(t, result.Err)
	case <-time.After(5 * time.Second):
		t.Fatal("background turn blocked without an SSE subscriber")
	}

	meta, ok, err := env.store.GetMeta(context.Background(), req.RunID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, run.StatusDone, meta.Status)
	require.Equal(t, req.RunID, env.mysql.saveTurnUsageArg.TraceID)
	batches := env.redis.dualBatchSnapshot()
	require.NotEmpty(t, batches)
	require.Equal(t, req.RunID+":0", batches[0][0].ClientMsgID)
}

func TestBackgroundSubmit_BusyClaimDoesNotStart(t *testing.T) {
	stub := doneStub()
	env := newPersistTestEnv(t, stub, nil)
	ctx := context.Background()
	_, claimed, err := env.store.ClaimSession(ctx, "sess-1", "interactive-run")
	require.NoError(t, err)
	require.True(t, claimed)

	submission := env.streamH.TurnExecutor().Submit(ctx, backgroundRequest(t, "scheduled-run-busy"))
	require.Equal(t, turnexec.SubmitSessionBusy, submission.Outcome)
	require.ErrorIs(t, submission.Err, turnexec.ErrSessionBusy)
	require.Nil(t, stub.gotMessages)
	require.Zero(t, env.mysql.appendMessagesCalls)
	active, err := env.store.ActiveRuns(ctx, []string{"sess-1"})
	require.NoError(t, err)
	require.Equal(t, "interactive-run", active["sess-1"])
}

func TestBackgroundSubmit_NonEmptyFIFODoesNotStart(t *testing.T) {
	stub := doneStub()
	env := newPersistTestEnv(t, stub, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	first := run.QueueEntry{RunID: "chat-running", SessionID: "sess-1", UserID: "user-1", Content: "first", EnqueuedAt: now}
	res, err := env.store.AdmitMessage(ctx, "sess-1", first, 8)
	require.NoError(t, err)
	require.Equal(t, run.AdmitClaimed, res.Outcome)
	queued := run.QueueEntry{RunID: "chat-queued", SessionID: "sess-1", UserID: "user-1", Content: "second", EnqueuedAt: now}
	res, err = env.store.AdmitMessage(ctx, "sess-1", queued, 8)
	require.NoError(t, err)
	require.Equal(t, run.AdmitQueued, res.Outcome)
	// The running chat finished but its queued successor has not yet pumped.
	require.NoError(t, env.store.ReleaseSession(ctx, "sess-1", "chat-running"))

	submission := env.streamH.TurnExecutor().Submit(ctx, backgroundRequest(t, "scheduled-run-fifo"))
	require.Equal(t, turnexec.SubmitSessionBusy, submission.Outcome)
	require.Nil(t, stub.gotMessages)
	depth, err := env.store.QueueLen(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, 1, depth, "background admission must not consume or bypass the FIFO")
}

type backgroundClaimErrorStore struct {
	run.Store
	err error
}

func (s *backgroundClaimErrorStore) ClaimBackgroundSession(context.Context, string, string) (string, bool, error) {
	return "", false, s.err
}

func TestBackgroundSubmit_RedisFailureIsFailClosed(t *testing.T) {
	stub := doneStub()
	env := newPersistTestEnv(t, stub, nil)
	env.streamH.runs.Store = &backgroundClaimErrorStore{Store: env.store, err: errors.New("redis down")}

	submission := env.streamH.TurnExecutor().Submit(context.Background(), backgroundRequest(t, "scheduled-run-redis"))
	require.Equal(t, turnexec.SubmitAdmissionFailed, submission.Outcome)
	require.ErrorIs(t, submission.Err, turnexec.ErrAdmissionUnavailable)
	require.Nil(t, stub.gotMessages)
	require.Zero(t, env.mysql.appendMessagesCalls)
}

func TestBackgroundSubmit_FenceRejectionReleasesClaimBeforeWrites(t *testing.T) {
	stub := doneStub()
	env := newPersistTestEnv(t, stub, nil)
	req := backgroundRequest(t, "scheduled-run-fence")
	fenceErr := errors.New("task paused")
	req.BeforeStart = func(context.Context) error { return fenceErr }

	submission := env.streamH.TurnExecutor().Submit(context.Background(), req)
	require.Equal(t, turnexec.SubmitRejected, submission.Outcome)
	require.ErrorIs(t, submission.Err, fenceErr)
	require.Nil(t, stub.gotMessages)
	require.Zero(t, env.mysql.appendMessagesCalls, "fence rejection precedes the user-row write")
	active, err := env.store.ActiveRuns(context.Background(), []string{"sess-1"})
	require.NoError(t, err)
	require.Empty(t, active["sess-1"], "the temporary background claim must be released")
	_, ok, err := env.store.GetMeta(context.Background(), req.RunID)
	require.NoError(t, err)
	require.False(t, ok, "fence rejection precedes run metadata publication")
}
