package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
)

// Liang is the analysis agent. Per config it has no tools, so Run makes a
// single non-tool streaming call and returns. If a future config grants Liang
// tools, the loop below generalises — but per the spec Liang is intentionally
// tool-less and never dispatches sub-agents.
type Liang struct {
	cfg    config.AgentConfig
	client LLMClient
}

// NewLiang builds a Liang agent. reg is accepted for signature symmetry with
// the other agents but is unused today; Liang has no tools.
func NewLiang(cfg config.AgentConfig, client LLMClient) (*Liang, error) {
	return &Liang{cfg: cfg, client: client}, nil
}

// Name implements Agent.
func (l *Liang) Name() string { return l.cfg.Name }

// SystemPrompt implements Agent.
func (l *Liang) SystemPrompt() string { return l.cfg.SystemPrompt }

// Run executes a single streaming chat completion with no tools and returns.
func (l *Liang) Run(ctx context.Context, messages []Message, hub *stream.Hub) (string, Usage, error) {
	select {
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	default:
	}

	if !hub.SendCtx(ctx, stream.AgentStartEvent(l.Name())) {
		return "", Usage{}, ctx.Err()
	}

	req := LLMRequest{
		Model:     l.cfg.Model,
		Messages:  withSystem(l.cfg.SystemPrompt, messages),
		MaxTokens: l.cfg.MaxTokens,
		// Tools intentionally omitted: cfg.Tools is empty per spec, so we
		// send no tools[] field. The client renders this as absent (not even
		// "[]"), which TestLiang_NoTools_PassesEmptyToolsJSON verifies.
	}

	var content string
	resp, err := l.client.StreamChat(ctx, req, func(delta string) error {
		content += delta
		if !hub.SendCtx(ctx, stream.TokenEvent(l.Name(), delta)) {
			return ctx.Err()
		}
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return content, resp.Usage, ctxErr
		}
		hub.SendCtx(ctx, stream.AgentErrorEvent(l.Name(), err.Error(), "llm_error"))
		hub.SendCtx(ctx, stream.AgentEndEvent(l.Name()))
		return content, resp.Usage, fmt.Errorf("liang: stream chat: %w", err)
	}

	if !hub.SendCtx(ctx, stream.AgentEndEvent(l.Name())) {
		return content, resp.Usage, ctx.Err()
	}
	if content == "" {
		content = resp.Content
	}
	return content, resp.Usage, nil
}
