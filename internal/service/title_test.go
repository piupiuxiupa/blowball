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
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/trace"
)

func newTitleSvc(m *fakeMySQLStore, llm agent.LLMClient) *TitleService {
	return NewTitleService(llm, m, config.OpenAIConfig{TitleModel: "gpt-4o-mini"})
}

func TestGenerateTitle_Success(t *testing.T) {
	const sessionID = "s-1"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "Login Bug Fix"}}
	svc := newTitleSvc(m, llm)

	ctx := trace.WithContext(context.Background(), "tid-1")
	svc.GenerateTitle(ctx, sessionID, "how do I fix the login bug?", "it broke again after patching")

	require.Equal(t, 1, m.upsertTitleCalls, "UpsertTitle must be called exactly once")
	assert.Equal(t, sessionID, m.upsertTitleArg.SessionID)
	assert.Equal(t, "Login Bug Fix", m.upsertTitleArg.Title)
	assert.Equal(t, "tid-1", m.upsertTitleArg.TraceID)

	require.True(t, llm.gotCall)
	require.Len(t, llm.lastReq.Messages, 2)
	assert.Equal(t, "system", llm.lastReq.Messages[0].Role)
	assert.Equal(t, "user", llm.lastReq.Messages[1].Role)
	// The prompt carries BOTH user anchors (first + current) and no
	// assistant content (title-generation-cadence: send-time input shape).
	assert.Contains(t, llm.lastReq.Messages[1].Content, "how do I fix the login bug?")
	assert.Contains(t, llm.lastReq.Messages[1].Content, "it broke again after patching")
	assert.NotContains(t, llm.lastReq.Messages[1].Content, "Assistant", "assistant content must not be part of the title input")
}

func TestGenerateTitle_LLMFailure_FallbacksToFirst20CharsOfCurrentUserMsg(t *testing.T) {
	const sessionID = "s-2"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{err: errors.New("network down")}
	svc := newTitleSvc(m, llm)

	currentMsg := strings.Repeat("a", 30) // 30 chars; fallback should yield first 20.
	svc.GenerateTitle(context.Background(), sessionID, "irrelevant first message", currentMsg)

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, strings.Repeat("a", 20), m.upsertTitleArg.Title, "fallback must be first 20 chars of the triggering message")
}

func TestGenerateTitle_TruncatesTo20Chars(t *testing.T) {
	const sessionID = "s-3"
	m := &fakeMySQLStore{}
	long := strings.Repeat("z", 50)
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: long}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "first question", "current question")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, 20, len([]rune(m.upsertTitleArg.Title)), "stored title must be exactly 20 chars")
	assert.Equal(t, strings.Repeat("z", 20), m.upsertTitleArg.Title)
}

func TestGenerateTitle_LLMReturnsEmpty_FallsBack(t *testing.T) {
	const sessionID = "s-4"
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "   "}}
	svc := newTitleSvc(m, llm)

	currentMsg := "short msg"
	svc.GenerateTitle(context.Background(), sessionID, "an earlier question", currentMsg)

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, currentMsg, m.upsertTitleArg.Title, "fallback must come from the triggering message, not the first one")
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
		svc.GenerateTitle(context.Background(), sessionID, "u", "c")
	})
	require.Equal(t, 1, m.upsertTitleCalls)
}

func TestGenerateTitle_ManualTitle_SkipsLLM(t *testing.T) {
	const sessionID = "s-manual"
	m := &fakeMySQLStore{getTitleFound: &model.Title{SessionID: sessionID, Title: "User Title", IsManual: true}}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "AI Title"}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "first question", "current question")

	require.Equal(t, 0, m.upsertTitleCalls, "UpsertTitle must NOT be called for manual titles")
	require.False(t, llm.gotCall, "LLM must NOT be called for manual titles")
}

// TestGenerateTitle_ManualSetInFlight_AIUpsertHasZeroEffect pins the SQL
// manual-guard semantics (title-generation-cadence): the early-exit GetTitle
// check passed (no manual title yet), then the user sets one while the LLM
// call is still in flight — the AI upsert that lands afterwards must leave
// the manual row's title, trace_id and is_manual completely untouched.
func TestGenerateTitle_ManualSetInFlight_AIUpsertHasZeroEffect(t *testing.T) {
	const sessionID = "s-inflight"
	m := &fakeMySQLStore{}
	llm := &inFlightManualLLM{store: m, sid: sessionID, manualTitle: "Manual In Flight", resp: agent.LLMResponse{Content: "AI Title"}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "first question", "current question")

	// Both writes executed: the in-flight manual upsert (from the LLM fake's
	// side effect) and the AI upsert — which runs but has zero effect on the
	// manual row (the fake mirrors the SQL guard).
	require.Equal(t, 2, m.upsertTitleCalls)
	assert.Equal(t, "AI Title", m.upsertTitleArg.Title, "the recorded (last) call is the AI upsert")

	m.mu.Lock()
	defer m.mu.Unlock()
	got := m.titles[sessionID]
	assert.Equal(t, "Manual In Flight", got.Title, "manual title must survive the in-flight AI upsert")
	assert.True(t, got.IsManual, "is_manual must stay TRUE")
	assert.Equal(t, "manual-tid", got.TraceID, "trace_id must stay the manual write's, not the AI generation's")
}

// TestUpsertTitle_NonManualRow_OverwrittenByAIUpsert pins the other half of
// the guard: an existing is_manual = FALSE row is still replaced by a later
// AI upsert (title + trace_id), and is_manual stays FALSE.
func TestUpsertTitle_NonManualRow_OverwrittenByAIUpsert(t *testing.T) {
	const sessionID = "s-ai-row"
	m := &fakeMySQLStore{}
	require.NoError(t, m.UpsertTitle(context.Background(), model.Title{SessionID: sessionID, Title: "Old AI Title", TraceID: "tid-old"}))

	require.NoError(t, m.UpsertTitle(context.Background(), model.Title{SessionID: sessionID, Title: "New AI Title", TraceID: "tid-new"}))

	m.mu.Lock()
	defer m.mu.Unlock()
	got := m.titles[sessionID]
	assert.Equal(t, "New AI Title", got.Title)
	assert.Equal(t, "tid-new", got.TraceID)
	assert.False(t, got.IsManual)
}

// inFlightManualLLM is an agent.LLMClient that stores a manual title through
// the fake store on every StreamChat call — simulating the user editing the
// title while the AI generation's LLM call is in flight (i.e. after the
// GetTitle early-exit check has already passed).
type inFlightManualLLM struct {
	store       *fakeMySQLStore
	sid         string
	manualTitle string
	resp        agent.LLMResponse
}

func (c *inFlightManualLLM) StreamChat(ctx context.Context, _ agent.LLMRequest, _ func(string) error, _ func(string) error) (agent.LLMResponse, error) {
	if err := c.store.UpsertTitleManual(ctx, model.Title{
		SessionID: c.sid,
		Title:     c.manualTitle,
		TraceID:   "manual-tid",
		IsManual:  true,
	}); err != nil {
		return agent.LLMResponse{}, err
	}
	return c.resp, nil
}

func TestGenerateTitle_GetTitleError_StillGenerates(t *testing.T) {
	const sessionID = "s-get-err"
	m := &fakeMySQLStore{getTitleErr: errors.New("db down")}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "Fallback Gen"}}
	svc := newTitleSvc(m, llm)

	svc.GenerateTitle(context.Background(), sessionID, "first question", "current question")

	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, "Fallback Gen", m.upsertTitleArg.Title)
	assert.True(t, llm.gotCall)
}

func TestSetManualTitle_Success(t *testing.T) {
	const sessionID = "s-set-manual"
	m := &fakeMySQLStore{}
	svc := newTitleSvc(m, nil)

	ctx := trace.WithContext(context.Background(), "tid-set")
	sanitized, err := svc.SetManualTitle(ctx, sessionID, "  My Manual Title  ")

	require.NoError(t, err)
	assert.Equal(t, "My Manual Title", sanitized)
	require.Equal(t, 1, m.upsertTitleCalls)
	assert.Equal(t, sessionID, m.upsertTitleArg.SessionID)
	assert.Equal(t, "My Manual Title", m.upsertTitleArg.Title)
	assert.True(t, m.upsertTitleArg.IsManual)
	assert.Equal(t, "tid-set", m.upsertTitleArg.TraceID)
}

func TestSetManualTitle_TruncatesTo20Runes(t *testing.T) {
	const sessionID = "s-truncate"
	m := &fakeMySQLStore{}
	svc := newTitleSvc(m, nil)

	_, err := svc.SetManualTitle(context.Background(), sessionID, strings.Repeat("x", 50))

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("x", 20), m.upsertTitleArg.Title)
}

func TestSetManualTitle_EmptyFallsBackToEmpty(t *testing.T) {
	const sessionID = "s-empty"
	m := &fakeMySQLStore{}
	svc := newTitleSvc(m, nil)

	_, err := svc.SetManualTitle(context.Background(), sessionID, "   ")

	require.NoError(t, err)
	assert.Empty(t, m.upsertTitleArg.Title)
}

func TestSetManualTitle_UpsertError_ReturnsError(t *testing.T) {
	const sessionID = "s-upsert-err"
	m := &fakeMySQLStore{upsertTitleErr: errors.New("dup")}
	svc := newTitleSvc(m, nil)

	_, err := svc.SetManualTitle(context.Background(), sessionID, "title")

	require.Error(t, err)
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
			svc.GenerateTitle(context.Background(), sessionID, "u", "c")
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
