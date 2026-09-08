package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
)

// fakeLLMTokenStore implements both service.LLMTokenStore and
// agent.UserLLMTokenStore over one in-memory map.
type fakeLLMTokenStore struct {
	mu   sync.Mutex
	rows map[string]model.UserLLMCredential
}

func (f *fakeLLMTokenStore) GetUserLLMCredential(_ context.Context, userID string) (*model.UserLLMCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cred, ok := f.rows[userID]
	if !ok {
		return nil, nil
	}
	return &cred, nil
}

func (f *fakeLLMTokenStore) UpsertUserLLMCredential(_ context.Context, cred model.UserLLMCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = make(map[string]model.UserLLMCredential)
	}
	f.rows[cred.UserID] = cred
	return nil
}

func (f *fakeLLMTokenStore) DeleteUserLLMCredential(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, userID)
	return nil
}

func (f *fakeLLMTokenStore) UserLLMToken(ctx context.Context, userID string) (string, error) {
	cred, err := f.GetUserLLMCredential(ctx, userID)
	if err != nil || cred == nil {
		return "", err
	}
	return cred.APIKey, nil
}

// userClientRecorder captures the API key each per-user client was built
// with; the client itself is a plain fakeLLMClient so call-site tests can
// observe which path a summary/title call took.
type userClientRecorder struct {
	mu   sync.Mutex
	keys []string
	llm  *fakeLLMClient
}

func (r *userClientRecorder) factory(cfg config.OpenAIConfig, _ agent.RawCaptureSink) agent.LLMClient {
	r.mu.Lock()
	r.keys = append(r.keys, cfg.APIKey)
	r.mu.Unlock()
	return r.llm
}

func (r *userClientRecorder) apiKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func TestLLMTokenService_StatusAndMasking(t *testing.T) {
	store := &fakeLLMTokenStore{}
	svc := NewLLMTokenService(store)

	configured, masked, err := svc.Status(context.Background(), "user-1")
	require.NoError(t, err)
	assert.False(t, configured)
	assert.Empty(t, masked)

	require.NoError(t, store.UpsertUserLLMCredential(context.Background(), model.UserLLMCredential{
		UserID: "user-1",
		APIKey: "sk-abcd1234efgh",
	}))
	configured, masked, err = svc.Status(context.Background(), "user-1")
	require.NoError(t, err)
	assert.True(t, configured)
	assert.Equal(t, "***efgh", masked)
}

func TestLLMTokenService_SaveValidatesTrimsAndOverwrites(t *testing.T) {
	store := &fakeLLMTokenStore{}
	svc := NewLLMTokenService(store)

	masked, err := svc.Save(context.Background(), "user-1", "  sk-first-token  ")
	require.NoError(t, err)
	assert.Equal(t, "***oken", masked)
	cred, err := store.GetUserLLMCredential(context.Background(), "user-1")
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, "sk-first-token", cred.APIKey, "Save must trim surrounding whitespace")

	// Blank after trimming is rejected and leaves the stored value intact.
	_, err = svc.Save(context.Background(), "user-1", "   ")
	require.ErrorIs(t, err, ErrInvalidLLMToken)
	cred, _ = store.GetUserLLMCredential(context.Background(), "user-1")
	assert.Equal(t, "sk-first-token", cred.APIKey)

	// Oversize is rejected (512-character column bound).
	_, err = svc.Save(context.Background(), "user-1", strings.Repeat("x", 513))
	require.ErrorIs(t, err, ErrInvalidLLMToken)

	// A valid save overwrites idempotently.
	_, err = svc.Save(context.Background(), "user-1", "sk-second-token-xyz")
	require.NoError(t, err)
	cred, _ = store.GetUserLLMCredential(context.Background(), "user-1")
	assert.Equal(t, "sk-second-token-xyz", cred.APIKey)
}

func TestLLMTokenService_DeleteFallsBackToUnconfigured(t *testing.T) {
	store := &fakeLLMTokenStore{}
	svc := NewLLMTokenService(store)
	_, err := svc.Save(context.Background(), "user-1", "sk-token-to-delete")
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "user-1"))
	configured, masked, err := svc.Status(context.Background(), "user-1")
	require.NoError(t, err)
	assert.False(t, configured)
	assert.Empty(t, masked)

	// Deleting an absent token succeeds.
	require.NoError(t, svc.Delete(context.Background(), "user-1"))
}

func TestMaskLLMToken_NeverRevealsShortTokens(t *testing.T) {
	assert.Equal(t, "***", MaskLLMToken("short"))
	assert.Equal(t, "***", MaskLLMToken("12345678"))
	assert.Equal(t, "***6789", MaskLLMToken("123456789"))
	assert.Equal(t, "***efgh", MaskLLMToken("sk-abcd1234efgh"))
}

func TestGenerateTitle_UsesUserTokenClient(t *testing.T) {
	// user-llm-token: the title call must bill to the triggering user's
	// credential. GenerateTitle derives a fresh background context — the
	// user id must be copied across (service/title.go) and the resolver must
	// build the per-user client from it.
	store := &fakeLLMTokenStore{}
	require.NoError(t, store.UpsertUserLLMCredential(context.Background(), model.UserLLMCredential{
		UserID: "user-title",
		APIKey: "sk-title-token",
	}))
	rec := &userClientRecorder{llm: &fakeLLMClient{resp: agent.LLMResponse{Content: "User Token Title"}}}
	resolver := agent.NewClientResolver(&fakeLLMClient{}, config.OpenAIConfig{}, nil, store)
	resolver.SetClientFactory(rec.factory)
	m := &fakeMySQLStore{}
	svc := newTitleSvc(m, resolver)

	svc.GenerateTitle(agent.WithUserID(context.Background(), "user-title"), "s-title", "first question", "latest question")

	require.Equal(t, []string{"sk-title-token"}, rec.apiKeys())
	assert.Equal(t, "User Token Title", m.upsertTitleArg.Title)
}

func TestCompactionService_Compact_UsesUserTokenClient(t *testing.T) {
	// user-llm-token: the checkpoint summary call runs in the turn context
	// (round hook or turn-start path), which carries the user identity — the
	// resolver must route it through the user's credential.
	store := &fakeLLMTokenStore{}
	require.NoError(t, store.UpsertUserLLMCredential(context.Background(), model.UserLLMCredential{
		UserID: "user-1",
		APIKey: "sk-compaction-token",
	}))
	rec := &userClientRecorder{llm: &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}}
	resolver := agent.NewClientResolver(&fakeLLMClient{}, config.OpenAIConfig{}, nil, store)
	resolver.SetClientFactory(rec.factory)

	rows, msgs, lastRow := compactionFixture(50)
	svc := newCompactionTestService(&fakeMySQLStore{}, &fakeRedisStore{}, resolver, 1000)
	rec2, ok := svc.Compact(agent.WithUserID(context.Background(), "user-1"), compactionInput(rows, msgs, lastRow))
	require.True(t, ok)
	require.NotNil(t, rec2)
	require.Equal(t, []string{"sk-compaction-token"}, rec.apiKeys())
}
