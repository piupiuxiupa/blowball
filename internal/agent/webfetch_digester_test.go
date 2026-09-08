package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
)

func TestNewWebfetchPromptClient_ResolvesCatalogAndSendsPrompt(t *testing.T) {
	cfg := config.OpenAIConfig{
		DefaultModel: "glm-4.7",
		Models: []config.ModelCatalogEntry{
			{Name: "gpt-5", MaxContextTokens: 400000, MaxCompletionTokens: 16384, Thinking: true},
			{Name: "glm-4.7", MaxContextTokens: 200000, MaxCompletionTokens: 8192, Thinking: false},
		},
	}
	client := newFake(fakeResponse{content: "digest output"})
	promptClient, err := NewWebfetchPromptClient(client, cfg, "gpt-5")
	require.NoError(t, err)

	out, err := promptClient.Prompt(context.Background(), "digest system", "digest user", 321)
	require.NoError(t, err)
	assert.Equal(t, "digest output", out)

	req := client.lastRequest()
	assert.Equal(t, "gpt-5", req.Model)
	assert.Equal(t, 321, req.MaxCompletionTokens)
	assert.True(t, req.Thinking)
	assert.Equal(t, "none", req.ReasoningEffort)
	require.Len(t, req.Messages, 2)
	assert.Equal(t, "system", req.Messages[0].Role)
	assert.Equal(t, "digest system", req.Messages[0].Content)
	assert.Equal(t, "user", req.Messages[1].Role)
	assert.Equal(t, "digest user", req.Messages[1].Content)
}

func TestNewWebfetchPromptClient_RejectsUnknownModel(t *testing.T) {
	cfg := config.OpenAIConfig{Models: []config.ModelCatalogEntry{
		{Name: "glm-4.7", MaxContextTokens: 200000, MaxCompletionTokens: 8192},
	}}
	_, err := NewWebfetchPromptClient(newFake(), cfg, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
	assert.Contains(t, err.Error(), "openai.models")
}

func TestWebfetchPromptClient_PromptUsesUserTokenClient(t *testing.T) {
	// user-llm-token: digestion runs inside the turn context, which carries
	// the user identity — so the digest call must resolve through the user's
	// credential, not the process-global one.
	cfg := config.OpenAIConfig{
		Models: []config.ModelCatalogEntry{
			{Name: "gpt-5", MaxContextTokens: 400000, MaxCompletionTokens: 16384, Thinking: true},
		},
	}
	tokens := &fakeTokenStore{}
	tokens.set("user-1", "sk-digest-token")
	factory := &recordingFactory{fake: newFake(fakeResponse{content: "digested"})}
	resolver := NewClientResolver(newFake(fakeResponse{content: "from fallback"}), cfg, nil, tokens)
	resolver.SetClientFactory(factory.build)

	promptClient, err := NewWebfetchPromptClient(resolver, cfg, "gpt-5")
	require.NoError(t, err)
	out, err := promptClient.Prompt(WithUserID(context.Background(), "user-1"), "digest system", "digest user", 100)
	require.NoError(t, err)
	assert.Equal(t, "digested", out)
	assert.Equal(t, []string{"sk-digest-token"}, factory.apiKeys())
}
