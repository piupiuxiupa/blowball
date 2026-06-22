package handler

import (
	"context"
	"sync"
	"testing"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/service"
)

// sessionDeps builds a service.SessionDeps from the three fakes. It is the
// handler-package counterpart of service.newDeps; the service package keeps
// its own copy private, so we re-expose the construction here for tests.
func sessionDeps(mysql service.MySQLStore, redis service.RedisStore, fs service.FSStore) service.SessionDeps {
	return service.SessionDeps{MySQL: mysql, Redis: redis, FS: fs}
}

// newSessionSvc wires a *service.SessionService from the bundled deps. Thin
// wrapper kept in the test package so each test reads as one line.
func newSessionSvc(deps service.SessionDeps) *service.SessionService {
	return service.NewSessionService(deps)
}

// newMessageSvc wires a *service.MessageService whose AppendMessage delegates
// to a freshly-built SessionService so the read path can be exercised without
// a separate write hook.
func newMessageSvc(deps service.SessionDeps) *service.MessageService {
	sessSvc := service.NewSessionService(deps)
	return service.NewMessageService(deps, sessSvc.SaveMessage)
}

// fakeTitleLLM is a tiny agent.LLMClient for TitleService tests. It returns a
// canned response on StreamChat and records the request so tests can assert.
type fakeTitleLLM struct {
	mu      sync.Mutex
	gotCall bool
	resp    agent.LLMResponse
}

func (c *fakeTitleLLM) StreamChat(_ context.Context, _ agent.LLMRequest, _ func(string) error, _ func(string) error) (agent.LLMResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gotCall = true
	return c.resp, nil
}

// newTitleSvcWithFake wires a *service.TitleService with a fake LLM that
// returns the supplied title content. The model name MUST be non-empty or
// TitleService short-circuits to the fallback.
func newTitleSvcWithFake(t *testing.T, deps service.SessionDeps, llmContent string) *service.TitleService {
	t.Helper()
	llm := &fakeTitleLLM{resp: agent.LLMResponse{Content: llmContent}}
	cfg := config.OpenAIConfig{Model: "title-model"}
	return service.NewTitleService(llm, deps.MySQL, cfg)
}
