package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
)

// fakeResolverTokens is the integration harness's in-memory
// agent.UserLLMTokenStore.
type fakeResolverTokens struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (f *fakeResolverTokens) UserLLMToken(_ context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[userID], nil
}

func (f *fakeResolverTokens) set(userID, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		f.tokens = make(map[string]string)
	}
	f.tokens[userID] = token
}

// resolverKeyRecorder records the API key every per-user client was built
// from and delegates the call to the shared scripted client, so the full
// HTTP → handler → orchestrator → resolver path is exercised while the
// resolved credential stays observable.
type resolverKeyRecorder struct {
	mu   sync.Mutex
	keys []string
	fake *scriptedLLMClient
}

func (r *resolverKeyRecorder) factory(cfg config.OpenAIConfig, _ agent.RawCaptureSink) agent.LLMClient {
	r.mu.Lock()
	r.keys = append(r.keys, cfg.APIKey)
	r.mu.Unlock()
	return r.fake
}

func (r *resolverKeyRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// waitForGeneratedTitle polls the in-memory MySQL fake until the session's
// async title generation (a goroutine) has landed its UpsertTitle.
func waitForGeneratedTitle(t *testing.T, env *testEnv, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env.mysqlFake.mu.Lock()
		_, ok := env.mysqlFake.titles[sessionID]
		env.mysqlFake.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for async title generation")
}

// TestUserLLMToken_ChatAndTitleUseUserTokenThenFallBack exercises the
// user-llm-token capability end to end through the real HTTP + orchestrator
// wiring: with a stored token, both the turn's chat round and the async title
// generation resolve through the per-user credential; after the token is
// removed, subsequent turns fall back to the deployment-global client with no
// further per-user client construction.
func TestUserLLMToken_ChatAndTitleUseUserTokenThenFallBack(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"user ", "client ", "reply"},
			content:      "user client reply",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13},
		},
		scriptedLLMResponse{
			tokens:       []string{"global ", "client ", "reply"},
			content:      "global client reply",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13},
		},
	)
	tokens := &fakeResolverTokens{}
	tokens.set(defaultUserID, "sk-integration-user-token")
	rec := &resolverKeyRecorder{fake: llm}
	resolver := agent.NewClientResolver(llm, config.OpenAIConfig{APIKey: "sk-global"}, nil, tokens)
	resolver.SetClientFactory(rec.factory)

	env := newTestEnv(t, resolver)
	token := authToken(t, defaultUserID)

	// Turn 1 with the user token configured: chat + async title both resolve
	// through the per-user credential.
	w := env.postMessage(`{"content":"hello with my own token"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	waitForGeneratedTitle(t, env, defaultSessionID)

	keys := rec.snapshot()
	require.NotEmpty(t, keys, "the chat round must resolve through the per-user client")
	for _, k := range keys {
		assert.Equal(t, "sk-integration-user-token", k, "every user-attributable LLM call (chat + title) must carry the user's token")
	}
	assert.GreaterOrEqual(t, llm.callCount(), 2, "chat round and title round must both reach the scripted client")

	// Delete the token (same observable state the DELETE endpoint produces)
	// and wait for the first turn's async title to fully settle before
	// snapshotting.
	tokens.set(defaultUserID, "")
	before := len(rec.snapshot())

	// Turn 2: no stored token → the global client answers, and no new
	// per-user client is constructed.
	w = env.postMessage(`{"content":"back to the global key"}`, token)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Len(t, rec.snapshot(), before, "no per-user client may be built after the token is deleted")
	assert.Greater(t, llm.callCount(), 2, "the fallback (global openai.api_key) client must serve the turn")
}
