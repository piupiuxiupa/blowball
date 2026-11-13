package handler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/service"
	"github.com/lush/blowball/pkg/stream"
	turnexec "github.com/lush/blowball/pkg/turn"
)

type scheduledSnapshotStore struct{}

func (*scheduledSnapshotStore) AppendSubAgentRun(context.Context, model.SubAgentInstance, model.SubAgentRun) error {
	return nil
}

type scheduledMessageStore struct {
	*handlerFakeMySQL
	userMessages []model.Message
}

func (s *scheduledMessageStore) AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error) {
	s.mu.Lock()
	for _, msg := range msgs {
		if msg.Role == model.RoleUser {
			s.userMessages = append(s.userMessages, msg)
		}
	}
	s.mu.Unlock()
	return s.handlerFakeMySQL.AppendMessages(ctx, msgs)
}

type scheduledOrchestrator struct {
	*stubOrchestrator
	snapshots agent.SubAgentSnapshotStore
}

func (s *scheduledOrchestrator) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []agent.Message, hub *stream.Hub, hooks TurnHooks, override agent.ModelOverride) ([]stream.StreamEvent, map[string]any, error) {
	s.snapshots = agent.SubAgentSnapshotStoreFromContext(ctx)
	return s.stubOrchestrator.Handle(ctx, workspaceRoot, skillsDir, userID, messages, hub, hooks, override)
}

type scheduledTitleClient struct {
	key   string
	calls chan string
}

func (c *scheduledTitleClient) StreamChat(_ context.Context, _ agent.LLMRequest, _ func(string) error, _ func(string) error) (agent.LLMResponse, error) {
	c.calls <- c.key
	return agent.LLMResponse{Content: "scheduled title"}, nil
}

type scheduledTitleTokenStore struct{}

func (*scheduledTitleTokenStore) UserLLMToken(_ context.Context, userID string) (string, error) {
	return "user-token-" + userID, nil
}

func TestMessageStreamScheduledServicesReachExecutor(t *testing.T) {
	ordinary := &handlerFakeMySQL{}
	ordinaryDeps := sessionDeps(ordinary, &handlerFakeRedis{}, newHandlerFakeFS())
	scheduled := &scheduledMessageStore{handlerFakeMySQL: &handlerFakeMySQL{
		getSessionByIDFound: &model.Session{SessionID: "sess-1", UserID: "user-1", Source: model.SessionSourceScheduled},
		listMessagesRows: []model.Message{{
			ID: 1, SessionID: "sess-1", Role: model.RoleAssistant, Agent: stream.AgentConfucius,
			EventType: model.EventTypeMessage, Content: "scheduled history", MsgTime: time.Now().UTC().Add(-time.Hour),
		}},
	}}
	scheduledDeps := sessionDeps(scheduled, &handlerFakeRedis{}, newHandlerFakeFS())
	scheduledSessions := service.NewSessionService(scheduledDeps)
	snapshots := &scheduledSnapshotStore{}
	titleCalls := make(chan string, 1)
	titleCfg := config.OpenAIConfig{TitleModel: "test-model"}
	titleClient := agent.NewClientResolver(
		&scheduledTitleClient{key: "global-token", calls: titleCalls},
		titleCfg, nil, &scheduledTitleTokenStore{},
	)
	titleClient.SetClientFactory(func(cfg config.OpenAIConfig, _ agent.RawCaptureSink) agent.LLMClient {
		return &scheduledTitleClient{key: cfg.APIKey, calls: titleCalls}
	})
	scheduledServices := &turnexec.ScheduledServices{
		Sessions:   scheduledSessions,
		Messages:   service.NewMessageService(scheduledDeps, scheduledSessions.SaveMessage),
		Titles:     service.NewTitleService(titleClient, scheduled, titleCfg),
		Compaction: service.NewCompactionService(scheduledDeps, nil, "test-model", 0),
		Snapshots:  snapshots,
	}
	stub := &scheduledOrchestrator{stubOrchestrator: doneStub()}
	runs := run.NewManager(run.NewMemStore(), run.NewRegistry())
	h := NewMessageStreamHandler(
		newSessionSvc(ordinaryDeps), newMessageSvc(ordinaryDeps), nil,
		service.NewCompactionService(ordinaryDeps, nil, "test-model", 0), nil, stub,
		t.TempDir(), runs, testSelectionConfig(), 0, scheduledServices,
	)
	req := backgroundRequest(t, "scheduled-source-run")
	req.Source = model.SessionSourceScheduled
	req.LLMContextPrefix = "pinned resource pointer"
	submission := h.TurnExecutor().Submit(t.Context(), req)
	require.Equal(t, turnexec.SubmitStarted, submission.Outcome, submission.Err)

	select {
	case result := <-submission.Started.ResultCh:
		require.NoError(t, result.Err)
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled turn blocked without a subscriber")
	}
	select {
	case key := <-titleCalls:
		assert.Equal(t, "user-token-"+req.UserID, key, "headless titles must use the task owner's credential")
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled title did not call its client")
	}

	batches, _ := scheduled.finalBatchSnapshots()
	require.NotEmpty(t, batches)
	scheduled.mu.Lock()
	userMessages := slices.Clone(scheduled.userMessages)
	scheduled.mu.Unlock()
	require.Len(t, userMessages, 1)
	assert.Equal(t, req.Content, userMessages[0].Content, "resource context must not alter the canonical user row")
	ordinaryBatches, _ := ordinary.finalBatchSnapshots()
	assert.Empty(t, ordinaryBatches)
	assert.Same(t, snapshots, stub.snapshots)
	require.NotEmpty(t, stub.gotMessages)
	assert.Equal(t, "scheduled history", stub.gotMessages[0].Content)
	assert.Equal(t, req.LLMContextPrefix+"\n\n"+req.Content, stub.gotMessages[len(stub.gotMessages)-1].Content)
	scheduled.mu.Lock()
	assert.Equal(t, 1, scheduled.saveTurnUsageCalls)
	scheduled.mu.Unlock()
	ordinary.mu.Lock()
	assert.Zero(t, ordinary.appendMessagesCalls)
	assert.Zero(t, ordinary.saveTurnUsageCalls)
	ordinary.mu.Unlock()
}
