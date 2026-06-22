package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/trace"
)

func newTitleSvc(m *fakeMySQLStore, llm agent.LLMClient) *TitleService {
	return NewTitleService(llm, m, config.OpenAIConfig{Model: "gpt-4o-mini"})
}

func TestGenerateTitle_Success(t *testing.T) {
	const sessionID = "s-1"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "Login Bug Fix"}}
	svc := newTitleSvc(m, llm)

	ctx := trace.WithContext(context.Background(), "tid-1")
	svc.GenerateTitle(ctx, sessionID, "how do I fix the login bug?", "you need to ...")

	require.Equal(t, 1, m.upsertTitleCalls, "UpsertTitle must be called exactly once")
	assert.Equal(t, sessionID, m.upsertTitleArg.SessionID)
	assert.Equal(t, "Login Bug Fix", m.upsertTitleArg.Title)
	assert.Equal(t, "tid-1", m.upsertTitleArg.TraceID)

	require.True(t, llm.gotCall)
	require.Len(t, llm.lastReq.Messages, 2)
	assert.Equal(t, "system", llm.lastReq.Messages[0].Role)
	assert.Equal(t, "user", llm.lastReq.Messages[1].Role)
	assert.Contains(t, llm.lastReq.Messages[1].Content, "how do I fix the login bug?")
}

func TestGenerateTitle_LLMFailure_FallbacksToFirst20CharsOfUserMsg(t *testing.T) {
	const sessionID = "s-2"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{err: errors.New("network down")}
	svc := newTitleSvc(m, llm)

	userMsg := strings.Repeat("a", 30) // 30 chars; fallback should yield first 20.
	svc.GenerateTitle(context.Background(), sessionID, userMsg, "any reply")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, strings.Repeat("a", 20), m.upsertTitleArg.Title, "fallback must be first 20 chars of userMsg")
}

func TestGenerateTitle_TruncatesTo20Chars(t *testing.T) {
	const sessionID = "s-3"
	m := &fakeMySQLStore{}
	long := strings.Repeat("z", 50)
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: long}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "user query", "assistant reply")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, 20, len([]rune(m.upsertTitleArg.Title)), "stored title must be exactly 20 chars")
	assert.Equal(t, strings.Repeat("z", 20), m.upsertTitleArg.Title)
}

func TestGenerateTitle_LLMReturnsEmpty_FallsBack(t *testing.T) {
	const sessionID = "s-4"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "   "}}
	svc := newTitleSvc(m, llm)

	userMsg := "short msg"
	svc.GenerateTitle(context.Background(), sessionID, userMsg, "reply")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, userMsg, m.upsertTitleArg.Title)
}

func TestGenerateTitle_LLMReturnsQuoted_StripsQuotes(t *testing.T) {
	const sessionID = "s-5"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: `"title with quotes"`}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "q", "r")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, "title with quotes", m.upsertTitleArg.Title)
}

func TestGenerateTitle_UpsertError_DoesNotPanic(t *testing.T) {
	const sessionID = "s-6"
	m := &fakeMySQLStore{upsertTitleErr: errors.New("dup")}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "ok"}}
	svc := newTitleSvc(m, llm)

	assert.NotPanics(t, func() {
		svc.GenerateTitle(context.Background(), sessionID, "u", "a")
	})
	require.Equal(t, 1, m.upsertTitleCalls)
}

func TestGenerateTitle_PanicRecovered(t *testing.T) {
	// A nil llm under the typed call path is one way the LLM call could go
	// wrong; combined with the explicit recover in GenerateTitle we should
	// observe the call returning without panicking the process.
	const sessionID = "s-7"
	m := &fakeMySQLStore{}

	// Force a panic by overriding the internal generate via a stub: we
	// instead inject an LLM that panics.
	llm := &panicLLMClient{}
	svc := newTitleSvc(m, llm)

	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NotPanics(t, func() {
			svc.GenerateTitle(context.Background(), sessionID, "u", "a")
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GenerateTitle did not return within 2s")
	}
}

// panicLLMClient is an agent.LLMClient whose StreamChat always panics.
type panicLLMClient struct{}

func (panicLLMClient) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	panic("boom")
}
