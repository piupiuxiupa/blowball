package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tokens"
)

// fakeOpenViking is a minimal OpenViking HTTP fake: it records every request
// (method, path, identity headers, decoded JSON body) and answers from the
// OV wire envelope. It exists to pin three things the real server guarantees
// us: the envelope shape, the identity headers, and the session call
// sequence.
type fakeOpenViking struct {
	mu       sync.Mutex
	requests []fakeRequest
	// handler overrides the canned responses when set (per-test scenarios).
	handler func(r fakeRequest) (int, any)
}

type fakeRequest struct {
	Method  string
	Path    string // path without query string
	Query   string // raw query string
	Headers http.Header
	Body    map[string]any
}

func (f *fakeOpenViking) record(r fakeRequest) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
}

func (f *fakeOpenViking) recorded() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeOpenViking) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: r.Header.Clone()}
	if r.Body != nil {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			req.Body = body
		}
	}
	f.record(req)

	if f.handler != nil {
		status, payload := f.handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	// Canned happy path: every API call succeeds with an empty-ish result.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": map[string]any{}})
}

func (f *fakeOpenViking) findMemories(r fakeRequest) (int, any) {
	if r.Path == "/api/v1/search/find" {
		return http.StatusOK, map[string]any{
			"status": "ok",
			"result": map[string]any{
				"memories": cannedMemories,
				"total":    len(cannedMemories),
			},
		}
	}
	return http.StatusOK, map[string]any{"status": "ok", "result": map[string]any{}}
}

var cannedMemories = []map[string]any{
	{"uri": "viking://user/memories/preferences/shell", "abstract": "User runs fish shell on macOS.", "score": 0.87},
	{"uri": "viking://user/memories/projects/blowball", "overview": "The user's main project is blowball, a Go multi-agent chat backend.", "score": 0.74},
	{"uri": "viking://user/memories/empty", "score": 0.5}, // no abstract, no overview: skipped
}

// newMemoriesFake returns the canned-memories fake with its handler bound as
// a method value (the struct-literal form cannot reference the receiver).
func newMemoriesFake() *fakeOpenViking {
	f := &fakeOpenViking{}
	f.handler = f.findMemories
	return f
}

func testService(t *testing.T, baseURL string) *Service {
	t.Helper()
	svc := NewService(config.MemoryConfig{
		Enabled:           true,
		BaseURL:           baseURL,
		APIKey:            "op-key",
		Account:           "blowball",
		RecallLimit:       6,
		RecallTokenBudget: 2000,
		RecallTimeout:     2 * time.Second,
		CaptureTimeout:    5 * time.Second,
		MaxCaptureBytes:   256 << 10,
	})
	require.NotNil(t, svc)
	return svc
}

func TestService_DisabledReturnsNil(t *testing.T) {
	if svc := NewService(config.MemoryConfig{Enabled: false, BaseURL: "http://x"}); svc != nil {
		t.Fatal("NewService(disabled) should return nil (zero-wiring shape)")
	}
	var nilSvc *Service
	if nilSvc.Enabled() {
		t.Error("nil Service should report Enabled() = false")
	}
	// Close is nil-safe.
	nilSvc.Close()
}

func TestOvIDs(t *testing.T) {
	// UUID v7 (what blowball mints) passes through verbatim.
	assert.Equal(t, "01928f3e-7c1a-7abc-8def-0123456789ab", ovUserID("01928f3e-7c1a-7abc-8def-0123456789ab"))
	assert.Equal(t, "bb-01928f3e-7c1a-7abc-8def-0123456789ab", ovSessionID("01928f3e-7c1a-7abc-8def-0123456789ab"))
	// Non-conforming ids hash deterministically and stay injective.
	a, b := ovUserID("user/one"), ovUserID("user/two")
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, "u-"))
	assert.Equal(t, a, ovUserID("user/one"))
	// Empty never passes through as a tenant id.
	assert.NotEqual(t, "", ovUserID(""))
}

func TestRecall_RendersBlock(t *testing.T) {
	fake := newMemoriesFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	block, err := svc.Recall(context.Background(), "01928f3e-7c1a-7abc-8def-0123456789ab", "which shell do I use?")
	require.NoError(t, err)

	assert.Contains(t, block, "<user-memories>")
	assert.Contains(t, block, "</user-memories>")
	assert.Contains(t, block, "fish shell on macOS")                       // abstract preferred
	assert.Contains(t, block, "blowball, a Go multi-agent chat backend")   // overview fallback
	assert.Contains(t, block, "[memory 87%]")
	assert.NotContains(t, block, "viking://user/memories/empty") // textless entry dropped
}

func TestRecall_EmptyAndError(t *testing.T) {
	// Empty memories → "" and no error (caller skips injection).
	fake := &fakeOpenViking{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	svc := testService(t, srv.URL)
	block, err := svc.Recall(context.Background(), "u1", "q")
	require.NoError(t, err)
	assert.Empty(t, block)

	// Server error → error returned for the caller to WARN.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","error":{"code":"INTERNAL","message":"boom"}}`))
	}))
	defer srv2.Close()
	svc2 := testService(t, srv2.URL)
	if _, err := svc2.Recall(context.Background(), "u1", "q"); err == nil {
		t.Fatal("Recall should surface server errors for the caller to WARN")
	}
}

func TestRecall_TokenBudget(t *testing.T) {
	long := map[string]any{
		"uri":      "viking://user/memories/long",
		"abstract": strings.Repeat("长记忆条目内容", 200), // 1200 CJK runes ≈ 1200 tokens
		"score":    0.9,
	}
	many := make([]map[string]any, 0, 5)
	for i := 0; i < 5; i++ {
		many = append(many, map[string]any{
			"uri":      fmt.Sprintf("viking://user/memories/m%d", i),
			"abstract": strings.Repeat("次级条目", 100), // 400 tokens each after entry cap
			"score":    0.5 - float64(i)*0.01,
		})
	}
	fake := &fakeOpenViking{handler: func(r fakeRequest) (int, any) {
		if r.Path == "/api/v1/search/find" {
			return http.StatusOK, map[string]any{"status": "ok", "result": map[string]any{"memories": append(many, long)}}
		}
		return http.StatusOK, map[string]any{"status": "ok", "result": map[string]any{}}
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	block, err := svc.Recall(context.Background(), "u1", "q")
	require.NoError(t, err)
	assert.NotEmpty(t, block)
	assert.LessOrEqual(t, tokens.Estimate(block), 2000+tokens.Estimate(recallLeadIn)+8,
		"the rendered block must respect the aggregate budget")
	assert.Contains(t, block, "长记忆条目内容", "the top-scored entry must always be included")
}

func TestRecall_RequestAndIdentityHeaders(t *testing.T) {
	fake := newMemoriesFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	_, err := svc.Recall(context.Background(), "01928f3e-7c1a-7abc-8def-0123456789ab", "q")
	require.NoError(t, err)

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, "POST", r.Method)
	assert.Equal(t, "/api/v1/search/find", r.Path)
	assert.Equal(t, "viking://user/memories", r.Body["target_uri"])
	assert.Equal(t, []any{"memory"}, r.Body["context_type"])
	assert.Equal(t, float64(6), r.Body["limit"])

	// Trusted-gateway identity headers (the isolation contract).
	assert.Equal(t, "01928f3e-7c1a-7abc-8def-0123456789ab", r.Headers.Get("X-OpenViking-User"))
	assert.Equal(t, "blowball", r.Headers.Get("X-OpenViking-Account"))
	assert.Equal(t, "op-key", r.Headers.Get("X-API-Key"))
}

func TestRecall_PerUserIsolationAndClientCache(t *testing.T) {
	fake := newMemoriesFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	_, err := svc.Recall(context.Background(), "user-a", "q")
	require.NoError(t, err)
	_, err = svc.Recall(context.Background(), "user-b", "q")
	require.NoError(t, err)
	_, err = svc.Recall(context.Background(), "user-a", "q2")
	require.NoError(t, err)

	reqs := fake.recorded()
	require.Len(t, reqs, 3)
	assert.Equal(t, "user-a", reqs[0].Headers.Get("X-OpenViking-User"))
	assert.Equal(t, "user-b", reqs[1].Headers.Get("X-OpenViking-User"))
	assert.Equal(t, "user-a", reqs[2].Headers.Get("X-OpenViking-User"))

	// One cached client per distinct user, reused across calls.
	svc.mu.Lock()
	assert.Len(t, svc.clients, 2)
	svc.mu.Unlock()
}

func TestCaptureTurn_Sequence(t *testing.T) {
	fake := &fakeOpenViking{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	userAt := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	endAt := userAt.Add(30 * time.Second)
	err := svc.CaptureTurn(context.Background(), TurnCapture{
		UserID:        "user-a",
		SessionID:     "01928f3e-7c1a-7abc-8def-0123456789ab",
		UserContent:   "记住我用 fish shell",
		AssistantText: "已记住:你使用 fish shell。",
		UserAt:        userAt,
		TurnEndAt:     endAt,
	})
	require.NoError(t, err)

	reqs := fake.recorded()
	require.Len(t, reqs, 3)
	// 1. get-or-create with auto_create.
	assert.Equal(t, "GET", reqs[0].Method)
	assert.Equal(t, "/api/v1/sessions/bb-01928f3e-7c1a-7abc-8def-0123456789ab", reqs[0].Path)
	assert.Equal(t, "auto_create=true", reqs[0].Query)
	assert.Equal(t, "user-a", reqs[0].Headers.Get("X-OpenViking-User"))
	// 2. batch with user+assistant, RFC3339 timestamps.
	assert.Equal(t, "POST", reqs[1].Method)
	assert.Equal(t, "/api/v1/sessions/bb-01928f3e-7c1a-7abc-8def-0123456789ab/messages/batch", reqs[1].Path)
	msgs, ok := reqs[1].Body["messages"].([]any)
	require.True(t, ok, "batch body must carry messages")
	require.Len(t, msgs, 2)
	first := msgs[0].(map[string]any)
	second := msgs[1].(map[string]any)
	assert.Equal(t, "user", first["role"])
	assert.Equal(t, "记住我用 fish shell", first["content"])
	assert.Equal(t, "2026-08-26T10:00:00Z", first["created_at"])
	assert.Equal(t, "assistant", second["role"])
	assert.Equal(t, "2026-08-26T10:00:30Z", second["created_at"])
	// 3. commit with keep_recent_count 0.
	assert.Equal(t, "POST", reqs[2].Method)
	assert.Equal(t, "/api/v1/sessions/bb-01928f3e-7c1a-7abc-8def-0123456789ab/commit", reqs[2].Path)
	assert.Equal(t, float64(0), reqs[2].Body["keep_recent_count"])
}

func TestCaptureTurn_EmptyAssistantAndTruncation(t *testing.T) {
	fake := &fakeOpenViking{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	svc := testService(t, srv.URL)
	// No assistant text → user message alone.
	err := svc.CaptureTurn(context.Background(), TurnCapture{
		UserID:      "user-a",
		SessionID:   "sess-1",
		UserContent: "just a question",
		UserAt:      time.Now(),
		TurnEndAt:   time.Now(),
	})
	require.NoError(t, err)
	reqs := fake.recorded()
	batch := reqs[1].Body["messages"].([]any)
	assert.Len(t, batch, 1, "empty assistant text must be skipped, not submitted blank")

	// Oversized content → truncated with a marker, rune-safely.
	huge := strings.Repeat("记", 200_000) // 600k bytes > 256KB cap
	svc2 := testService(t, srv.URL)
	err = svc2.CaptureTurn(context.Background(), TurnCapture{
		UserID:        "user-a",
		SessionID:     "sess-2",
		UserContent:   huge,
		AssistantText: huge,
		UserAt:        time.Now(),
		TurnEndAt:     time.Now(),
	})
	require.NoError(t, err)
	all := fake.recorded()
	lastBatch := all[len(all)-2].Body["messages"].([]any) // final capture's batch
	for i, m := range lastBatch {
		content := m.(map[string]any)["content"].(string)
		assert.Less(t, len(content), 256<<10+200, "message %d must be capped near max_capture_bytes", i)
		assert.Contains(t, content, "[blowball: truncated", "message %d must carry the truncation marker", i)
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	svc := testService(t, srv.URL)
	ok, err := svc.Health(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)
}
