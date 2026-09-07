package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

type fakeSubAgentRunStore struct {
	session   model.Session
	instance  model.SubAgentInstance
	runs      []model.SubAgentRun
	listCalls int
}

func (s *fakeSubAgentRunStore) GetSessionByID(context.Context, string) (*model.Session, error) {
	sess := s.session
	return &sess, nil
}

func (s *fakeSubAgentRunStore) GetSubAgentInstance(_ context.Context, _, instanceID string) (model.SubAgentInstance, bool, error) {
	return s.instance, instanceID == s.instance.AgentInstanceID, nil
}

func (s *fakeSubAgentRunStore) GetSubAgentRun(_ context.Context, _, instanceID, runID string) (model.SubAgentRun, bool, error) {
	for _, run := range s.runs {
		if run.AgentInstanceID == instanceID && run.RunID == runID {
			return run, true, nil
		}
	}
	return model.SubAgentRun{}, false, nil
}

func (s *fakeSubAgentRunStore) ListSubAgentRuns(context.Context, string, string) ([]model.SubAgentRun, error) {
	s.listCalls++
	return append([]model.SubAgentRun(nil), s.runs...), nil
}

func newSubAgentTranscriptFixture() *fakeSubAgentRunStore {
	start := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	return &fakeSubAgentRunStore{
		session: model.Session{SessionID: "sess-1", UserID: "user-1"},
		instance: model.SubAgentInstance{
			SessionID: "sess-1", AgentInstanceID: "inst-1", Name: "worker",
			SystemPrompt: "secret system prompt", LatestRunID: "run-2",
		},
		runs: []model.SubAgentRun{
			{
				SessionID: "sess-1", AgentInstanceID: "inst-1", RunID: "run-1",
				RunNo: 1, Name: "worker", Status: model.SubAgentStatusCompleted,
				SnapshotKind: model.SubAgentSnapshotDelta, MessageCount: 1,
				StartedAt: start, FinishedAt: end,
				MessagesJSON: []byte(`[{"role":"user","content":"first"}]`),
			},
			{
				SessionID: "sess-1", AgentInstanceID: "inst-1", RunID: "run-2",
				PreviousRunID: "run-1", RunNo: 2, Name: "worker",
				Status: model.SubAgentStatusCompleted, SnapshotKind: model.SubAgentSnapshotDelta,
				MessageCount: 3, StartedAt: end, FinishedAt: end.Add(time.Minute),
				MessagesJSON: []byte(`[
					{"role":"system","content":"secret"},
					{"role":"user","content":"continue"},
					{"role":"assistant","content":"done","reasoning_content":"thought",
					 "tool_calls":[{"id":"call-tool","function":{"name":"executor","arguments":"{\"command\":\"ls\"}"}}]},
					{"role":"tool","tool_call_id":"call-tool","name":"executor","content":"files"},
					{"role":"unknown","content":"future"}
				]`),
			},
		},
	}
}

func TestSubAgentService_ListRunsOwnershipAndThinPayload(t *testing.T) {
	fixture := newSubAgentTranscriptFixture()
	svc := NewSubAgentService(fixture)

	runs, err := svc.ListRuns(context.Background(), "user-1", "sess-1", "inst-1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-2", runs[1].RunID)
	assert.Equal(t, 3, runs[1].MessageCount)

	fixture.listCalls = 0
	_, err = svc.ListRuns(context.Background(), "user-2", "sess-1", "inst-1")
	assert.ErrorIs(t, err, ErrSubAgentNotFound)
	assert.Zero(t, fixture.listCalls, "cross-user access stops before run listing")
}

func TestSubAgentService_GetRunProjectsSafeTranscript(t *testing.T) {
	svc := NewSubAgentService(newSubAgentTranscriptFixture())

	got, err := svc.GetRun(context.Background(), "user-1", "sess-1", "inst-1", "run-2")
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	require.Len(t, got.Transcript, 3)
	assert.Equal(t, "task", got.Transcript[0].Type)
	assert.Equal(t, "continue", got.Transcript[0].Content)
	assert.Equal(t, "assistant", got.Transcript[1].Type)
	assert.Equal(t, "thought", got.Transcript[1].ReasoningContent)
	require.Len(t, got.Transcript[1].ToolCalls, 1)
	assert.Equal(t, SubAgentToolCall{
		ID: "call-tool", Name: "executor", Arguments: []byte(`{"command":"ls"}`),
	}, got.Transcript[1].ToolCalls[0])
	assert.Equal(t, SubAgentTranscriptItem{
		Type: "tool_result", Content: "files", ToolCallID: "call-tool", Name: "executor",
	}, got.Transcript[2])
	assert.NotEqual(t, "secret", got.Transcript[0].Content)
}

func TestSubAgentService_GetRunMissingAndMismatched(t *testing.T) {
	svc := NewSubAgentService(newSubAgentTranscriptFixture())
	_, err := svc.GetRun(context.Background(), "user-1", "sess-1", "inst-1", "missing")
	assert.ErrorIs(t, err, ErrSubAgentNotFound)

	_, err = svc.GetRun(context.Background(), "user-2", "sess-1", "inst-1", "run-1")
	assert.ErrorIs(t, err, ErrSubAgentNotFound)
}
