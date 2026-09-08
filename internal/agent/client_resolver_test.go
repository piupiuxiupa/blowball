package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
)

// fakeTokenStore is a mutable in-memory UserLLMTokenStore.
type fakeTokenStore struct {
	mu     sync.Mutex
	tokens map[string]string
	err    error
}

func (f *fakeTokenStore) UserLLMToken(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.tokens[userID], nil
}

func (f *fakeTokenStore) set(userID, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		f.tokens = make(map[string]string)
	}
	f.tokens[userID] = token
}

// recordingFactory captures the config each per-user client was built from
// and routes the call to a dedicated fake so tests can tell user-client calls
// apart from fallback calls.
type recordingFactory struct {
	mu    sync.Mutex
	keys  []string
	calls int
	fake  *fakeLLMClient
}

func (r *recordingFactory) build(cfg config.OpenAIConfig, _ RawCaptureSink) LLMClient {
	r.mu.Lock()
	r.keys = append(r.keys, cfg.APIKey)
	r.calls++
	r.mu.Unlock()
	return r.fake
}

func (r *recordingFactory) apiKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func userCtx(userID string) context.Context {
	return WithUserID(context.Background(), userID)
}

func TestClientResolver_ConfiguredTokenUsesUserClient(t *testing.T) {
	tokens := &fakeTokenStore{}
	tokens.set("user-1", "sk-user-token")
	fallback := newFake(fakeResponse{content: "from fallback"})
	factory := &recordingFactory{fake: newFake(fakeResponse{content: "from user client"})}
	cfg := config.OpenAIConfig{APIKey: "sk-global", BaseURL: "https://gw.example"}

	r := NewClientResolver(fallback, cfg, nil, tokens)
	r.SetClientFactory(factory.build)

	resp, err := r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "from user client", resp.Content)
	assert.Equal(t, []string{"sk-user-token"}, factory.apiKeys(), "the per-user client must be built from the user's token")
	assert.Empty(t, fallback.calls, "the fallback client must not be consulted")

	// Only the credential axis changes; base_url and the rest of the config
	// ride through verbatim (asserted via a second factory capture).
	var gotBaseURL string
	r.SetClientFactory(func(c config.OpenAIConfig, _ RawCaptureSink) LLMClient {
		gotBaseURL = c.BaseURL
		return newFake(fakeResponse{content: "x"})
	})
	_, err = r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example", gotBaseURL)
}

func TestClientResolver_UnconfiguredFallsBackToGlobalClient(t *testing.T) {
	tokens := &fakeTokenStore{} // no token for anyone
	fallback := newFake(fakeResponse{content: "from fallback"}, fakeResponse{content: "from fallback"})
	factory := &recordingFactory{fake: newFake(fakeResponse{content: "from user client"})}

	r := NewClientResolver(fallback, config.OpenAIConfig{APIKey: "sk-global"}, nil, tokens)
	r.SetClientFactory(factory.build)

	resp, err := r.StreamChat(userCtx("user-unset"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "from fallback", resp.Content)
	assert.Empty(t, factory.apiKeys(), "no per-user client may be built without a stored token")

	// No user identity in ctx (e.g. a context that never passed through
	// Handle) behaves exactly like an unconfigured user.
	resp, err = r.StreamChat(context.Background(), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "from fallback", resp.Content)
}

func TestClientResolver_TokenUpdateTakesEffectOnNextCall(t *testing.T) {
	tokens := &fakeTokenStore{}
	tokens.set("user-1", "sk-first")
	factory := &recordingFactory{fake: newFake(fakeResponse{content: "x"}, fakeResponse{content: "x"})}
	r := NewClientResolver(newFake(fakeResponse{content: "y"}), config.OpenAIConfig{}, nil, tokens)
	r.SetClientFactory(factory.build)

	_, err := r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	tokens.set("user-1", "sk-second")
	_, err = r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"sk-first", "sk-second"}, factory.apiKeys())

	// Deleting the token (empty string) routes back to the fallback.
	fallback := newFake(fakeResponse{content: "from fallback"})
	r = NewClientResolver(fallback, config.OpenAIConfig{}, nil, tokens)
	r.SetClientFactory(factory.build)
	tokens.set("user-1", "")
	resp, err := r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "from fallback", resp.Content)
}

func TestClientResolver_StoreErrorFailsExplicitly(t *testing.T) {
	tokens := &fakeTokenStore{err: errors.New("mysql down")}
	fallback := newFake(fakeResponse{content: "from fallback"})
	r := NewClientResolver(fallback, config.OpenAIConfig{}, nil, tokens)

	_, err := r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve llm token")
	assert.Empty(t, fallback.calls, "a credential-store error must never silently fall back to the global key")
}

func TestClientResolver_NilTokenStoreUsesFallback(t *testing.T) {
	fallback := newFake(fakeResponse{content: "from fallback"})
	r := NewClientResolver(fallback, config.OpenAIConfig{}, nil, nil)
	resp, err := r.StreamChat(userCtx("user-1"), LLMRequest{Model: "gpt-5"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "from fallback", resp.Content)
}
