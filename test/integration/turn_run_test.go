package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
)

// gatedLLM holds every StreamChat call at a gate until released (or the turn
// context is cancelled), then delegates to the scripted inner client. It gives
// integration tests a turn that is observably "still running" while the test
// drives the turn-lifecycle endpoints.
type gatedLLM struct {
	inner       agent.LLMClient
	entered     chan struct{}
	gate        chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func newGatedLLM(inner agent.LLMClient) *gatedLLM {
	return &gatedLLM{inner: inner, entered: make(chan struct{}), gate: make(chan struct{})}
}

// release opens the gate (idempotent). Tests call it when they want the held
// calls to proceed; the testEnv cleanup also calls it so a parked call cannot
// outlive the harness's goroutine-leak check.
func (g *gatedLLM) release() {
	g.releaseOnce.Do(func() { close(g.gate) })
}

func (g *gatedLLM) StreamChat(ctx context.Context, req agent.LLMRequest, onToken func(string) error, onReasoning func(string) error) (agent.LLMResponse, error) {
	// The send-time title generation (title-generation-cadence) runs in
	// parallel with the turn and must NOT be held by the turn's gate — tests
	// gate the turn; title calls pass straight through to the inner client.
	if req.Model == scriptedTitleModel {
		return g.inner.StreamChat(ctx, req, onToken, onReasoning)
	}
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.gate:
		return g.inner.StreamChat(ctx, req, onToken, onReasoning)
	case <-ctx.Done():
		return agent.LLMResponse{}, ctx.Err()
	}
}

// postMessageCtx is postMessage with a caller-owned request context, so the
// test can simulate the originating client disconnecting mid-turn.
func (e *testEnv) postMessageCtx(ctx context.Context, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/"+defaultSessionID+"/messages",
		strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

func (e *testEnv) turnRequest(method, runID, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method,
		"/api/v1/sessions/"+defaultSessionID+"/turns/"+runID+"/events",
		nil)
	if method == http.MethodPost {
		req = httptest.NewRequest(method,
			"/api/v1/sessions/"+defaultSessionID+"/turns/"+runID+"/cancel",
			nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// busyRunID posts a competing message (expecting 409 SESSION_BUSY) and returns
// the active run id reported in the rejection — the discovery path a frontend
// uses when the user retries against a still-generating session.
func busyRunID(t *testing.T, env *testEnv, token string) string {
	t.Helper()
	w := env.postMessage(`{"content":"again"}`, token)
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct {
			Code  string `json:"code"`
			RunID string `json:"run_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "SESSION_BUSY", body.Error.Code)
	require.NotEmpty(t, body.Error.RunID)
	return body.Error.RunID
}

// TestTurnRun_DisconnectDoesNotCancelThenResumeReplay drives the core
// turn-detach-resume contract end to end with the real handler stack over
// miniredis: the originating client disconnects mid-turn (the turn keeps
// running), a concurrent message is rejected 409 carrying the run id, the
// resume endpoint live-tails the still-running turn, and after completion the
// session unlocks and the turn is persisted.
func TestTurnRun_DisconnectDoesNotCancelThenResumeReplay(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"Hello, ", "world!"},
			content:      "Hello, world!",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
		scriptedLLMResponse{ // the follow-up turn's assistant round
			tokens: []string{"Hi"}, content: "Hi", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	).withTitleResponses(
		scriptedLLMResponse{ // title round (send-time, races the gated turn call)
			content: "Greeting", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	gated := newScriptedGated(t, scripted)
	env := newTestEnv(t, gated)
	token := authToken(t, defaultUserID)

	// Start the turn with a disconnectable request context.
	ctx, cancel := context.WithCancel(context.Background())
	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() { done1 <- env.postMessageCtx(ctx, `{"content":"hello"}`, token) }()

	// Wait until the turn is observably running (the LLM call is gated).
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the LLM call")
	}

	// Session busy: a competing message is rejected with the active run id.
	runID := busyRunID(t, env, token)

	// The originating client disconnects. The turn must keep running: the
	// handler goroutine parks on the detached turn and the recorder does NOT
	// complete while the gate is still closed.
	cancel()
	select {
	case <-done1:
		t.Fatal("turn ended when the client disconnected — detach broken")
	case <-time.After(200 * time.Millisecond):
	}

	// Resume: a fresh subscriber attaches to the still-running turn.
	doneAttach := make(chan *httptest.ResponseRecorder, 1)
	go func() { doneAttach <- env.turnRequest(http.MethodGet, runID, token) }()

	// Let the attach subscriber observe the running stream, then release.
	time.Sleep(150 * time.Millisecond)
	gated.release()

	// Both subscriptions complete once the turn finishes.
	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Fatal("originating handler never completed after turn end")
	}
	select {
	case rec := <-doneAttach:
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		types, _ := parseSSEBody(t, rec.Body.String())
		assert.Contains(t, types, "agent_start")
		assert.Contains(t, types, "token")
		assert.Equal(t, "done", types[len(types)-1], "attach stream must end with done")
		assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		assert.Equal(t, runID, rec.Header().Get("X-Run-Id"))
	case <-time.After(10 * time.Second):
		t.Fatal("attach stream never completed")
	}

	// The detached turn still persisted through the write-behind path.
	env.waitForPersistedTurn(t, defaultSessionID, 1)

	// The session claim is released: a follow-up message is accepted.
	wNext := env.postMessage(`{"content":"next"}`, token)
	require.Equal(t, http.StatusOK, wNext.Code, "body: %s", wNext.Body.String())
}

// TestTurnRun_CancelEndpointCancelsRunningTurn drives the cancel endpoint
// against a turn gated mid-LLM-call: the in-process registry cancel fires, the
// turn ends as cancelled, partial output is persisted, and the session claim
// is released.
func TestTurnRun_CancelEndpointCancelsRunningTurn(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens:       []string{"partial"},
			content:      "partial",
			finishReason: "stop",
			usage:        agent.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		},
		scriptedLLMResponse{ // the follow-up turn's assistant round
			tokens: []string{"Hi"}, content: "Hi", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5},
		},
	).withTitleResponses(
		// Title round: fired at send time (before the turn started), so it
		// completes even though the turn itself is cancelled mid-call.
		scriptedLLMResponse{content: "T", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	gated := newScriptedGated(t, scripted)
	env := newTestEnv(t, gated)
	token := authToken(t, defaultUserID)

	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() { done1 <- env.postMessage(`{"content":"hello"}`, token) }()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the LLM call")
	}

	// Discover the run id through the busy rejection, then cancel by id.
	runID := busyRunID(t, env, token)
	wCancel := env.turnRequest(http.MethodPost, runID, token)
	require.Equal(t, http.StatusOK, wCancel.Code, "body: %s", wCancel.Body.String())
	require.Contains(t, wCancel.Body.String(), `"cancelling"`)

	// The turn winds down and the handler completes; partial output (the user
	// message) is persisted through the interrupted-turn path.
	select {
	case rec := <-done1:
		// The SSE response may have ended early (cancel has no done event in
		// the log) — the body is whatever was streamed before the cancel.
		_ = rec
	case <-time.After(10 * time.Second):
		t.Fatal("turn never ended after cancel")
	}
	env.waitForPersistedTurn(t, defaultSessionID, 1)

	// Run state: cancelled + session claim released (the next message is
	// accepted rather than 409).
	require.Eventually(t, func() bool {
		return env.miniRedis.HGet("run:"+runID+":meta", "status") == "cancelled"
	}, 5*time.Second, 50*time.Millisecond, "run meta never reached cancelled")
	require.Eventually(t, func() bool {
		_, err := env.miniRedis.Get("session:" + defaultSessionID + ":run")
		return err != nil // key gone
	}, 5*time.Second, 50*time.Millisecond, "session claim not released after cancel")

	// The claim key being gone IS the unlock proof; driving a full follow-up
	// turn here would need another gated script round for no extra coverage.

	// Release the gate so any in-flight residue can drain before the
	// harness's goroutine-leak check snapshots the process.
	gated.release()
	time.Sleep(250 * time.Millisecond)

	// The send-time title generation completed on its detached context even
	// though the turn itself was cancelled mid-call — the session keeps its
	// title (title-generation-cadence: a cancelled turn still has a title).
	env.mysqlFake.mu.Lock()
	assert.Equal(t, "T", env.mysqlFake.titles[defaultSessionID].Title, "cancelled turn must keep its send-time title")
	env.mysqlFake.mu.Unlock()
}

// TestTurnRun_SessionListGeneratingFlag asserts the list endpoint reflects
// the running turn: generating=true while the turn is gated, false after it
// completes.
func TestTurnRun_SessionListGeneratingFlag(t *testing.T) {
	scripted := newScriptedLLMClient(
		scriptedLLMResponse{
			tokens: []string{"hi"}, content: "hi", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		},
	).withTitleResponses(
		scriptedLLMResponse{ // title round
			content: "T", finishReason: "stop",
			usage: agent.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	)
	gated := newScriptedGated(t, scripted)
	env := newTestEnv(t, gated)
	token := authToken(t, defaultUserID)

	listGenerating := func() (generating bool, runID string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var body struct {
			Sessions []struct {
				SessionID  string `json:"session_id"`
				Generating bool   `json:"generating"`
				RunID      string `json:"run_id"`
			} `json:"sessions"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		for _, s := range body.Sessions {
			if s.SessionID == defaultSessionID {
				return s.Generating, s.RunID
			}
		}
		return false, ""
	}

	done := make(chan struct{})
	go func() {
		env.postMessage(`{"content":"hello"}`, token)
		close(done)
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the LLM call")
	}

	// While running: generating=true AND the entry carries the run id — the
	// reload discovery path (a reloaded page lost the X-Run-Id header it saw
	// when the turn started).
	generating, runID := listGenerating()
	require.True(t, generating, "session list must mark the running turn")
	require.NotEmpty(t, runID, "generating entry must carry run_id")
	claim, err := env.miniRedis.Get("session:" + defaultSessionID + ":run")
	require.NoError(t, err)
	require.Equal(t, claim, runID, "list run_id must equal the active claim")

	gated.release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never completed")
	}
	generating, runID = listGenerating()
	require.False(t, generating, "generating flag must clear after turn end")
	require.Empty(t, runID, "run_id must clear with the claim")
}

// newScriptedGated wraps a scripted client in a gatedLLM and releases the gate
// at cleanup so a parked call cannot outlive the harness's goroutine-leak
// check. Title-generation calls bypass the gate entirely (see StreamChat).
func newScriptedGated(t *testing.T, inner agent.LLMClient) *gatedLLM {
	t.Helper()
	g := newGatedLLM(inner)
	t.Cleanup(g.release)
	return g
}
