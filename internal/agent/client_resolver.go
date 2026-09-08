package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool/skill"
)

// UserLLMTokenStore reads a user's configured LLM gateway token
// (user-llm-token capability). It returns "" when the user has no token
// configured — the resolver then falls back to the deployment-global
// openai.api_key client. Any non-nil error is a hard failure: the resolver
// must never silently swap credentials.
type UserLLMTokenStore interface {
	UserLLMToken(ctx context.Context, userID string) (string, error)
}

// ClientResolver is an LLMClient that routes every call through the
// requesting user's credential when one is configured (user-llm-token).
//
// The userID is read from the call context — Orchestrator.Handle injects it
// for the whole turn (so Confucius, sub-agents, webfetch digestion and the
// mid-turn compaction round hook all inherit it), and the streaming handler
// injects it for the title-generation and turn-start compaction paths.
//
// Resolution semantics:
//   - no user identity in ctx, no token store, or no configured token
//     → the fallback (global openai.api_key) client, preserving pre-change
//     behavior byte for byte;
//   - configured token → a client built from the global openai.base_url and
//     the user's token (only the credential axis changes — base_url, model
//     catalog, effort and quotas stay deployment-global);
//   - token-store error → the call fails explicitly; there is no silent
//     fallback to the global credential.
//
// Tokens are resolved per call (one indexed primary-key read per LLM round —
// negligible next to the multi-second completion it precedes) so an update or
// delete takes effect on subsequent calls even across api/agent role
// processes sharing one MySQL. The constructed openai client is a stateless
// configuration shell, so per-call construction is equivalent to caching one
// instance per turn.
type ClientResolver struct {
	fallback LLMClient
	cfg      config.OpenAIConfig
	sink     RawCaptureSink
	tokens   UserLLMTokenStore
	// newUserClient builds the per-user client. Production defaults to
	// NewOpenAIClientWithSink; tests override it via SetClientFactory to
	// observe the resolved credential without a real gateway.
	newUserClient func(config.OpenAIConfig, RawCaptureSink) LLMClient
}

// NewClientResolver wraps fallback (the process-global openai.api_key client)
// with per-user token resolution against tokens. A nil tokens store disables
// resolution entirely — every call uses fallback unchanged.
func NewClientResolver(fallback LLMClient, cfg config.OpenAIConfig, sink RawCaptureSink, tokens UserLLMTokenStore) *ClientResolver {
	return &ClientResolver{
		fallback: fallback,
		cfg:      cfg,
		sink:     sink,
		tokens:   tokens,
		newUserClient: func(cfg config.OpenAIConfig, sink RawCaptureSink) LLMClient {
			return NewOpenAIClientWithSink(cfg, sink)
		},
	}
}

// SetClientFactory overrides per-user client construction. It exists for
// tests (unit and integration) that need to observe the resolved API key
// instead of dialing a real gateway; production never calls it.
func (r *ClientResolver) SetClientFactory(f func(config.OpenAIConfig, RawCaptureSink) LLMClient) {
	if r != nil && f != nil {
		r.newUserClient = f
	}
}

// StreamChat implements LLMClient. It resolves the credential for the user
// identified in ctx, then delegates. Resolution errors surface as call
// errors (the existing transient-error classifier sees a plain error, and the
// turn's agent_error channel reports it — never a silent global-key retry).
func (r *ClientResolver) StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error, onReasoning func(string) error) (LLMResponse, error) {
	client, err := r.resolve(ctx)
	if err != nil {
		return LLMResponse{}, err
	}
	return client.StreamChat(ctx, req, onToken, onReasoning)
}

// resolve picks the client for one call.
func (r *ClientResolver) resolve(ctx context.Context) (LLMClient, error) {
	if r == nil || r.tokens == nil {
		return r.fallback, nil
	}
	userID := skill.UserIDFromContext(ctx)
	if userID == "" {
		return r.fallback, nil
	}
	token, err := r.tokens.UserLLMToken(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("agent: resolve llm token for user: %w", err)
	}
	if token == "" {
		return r.fallback, nil
	}
	cfg := r.cfg
	cfg.APIKey = token
	return r.newUserClient(cfg, r.sink), nil
}
