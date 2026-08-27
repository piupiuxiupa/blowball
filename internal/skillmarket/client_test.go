package skillmarket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
)

// newTestClient builds a Client against srv with a tight timeout and an
// injectable clock, plus a ctx carrying the login token.
func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	cfg := config.SkillMarketConfig{
		URL:      url,
		Timeout:  2 * time.Second,
		CacheTTL: time.Minute,
	}
	c := New(cfg, "/data/skills-market")
	if c == nil {
		t.Fatal("New returned nil for an enabled config")
	}
	return c
}

func ctxWithToken(token string) context.Context {
	return WithToken(context.Background(), token)
}

// TestNew_DisabledReturnsNil verifies the zero-wiring contract: an empty url
// yields a nil client (callers keep the single nil-safe call shape).
func TestNew_DisabledReturnsNil(t *testing.T) {
	if c := New(config.SkillMarketConfig{URL: "", Timeout: time.Second, CacheTTL: time.Minute}, "/m"); c != nil {
		t.Fatalf("New with empty url = %v, want nil", c)
	}
	var nilc *Client
	if nilc.Enabled() {
		t.Error("nil client should report Enabled()=false")
	}
	if nilc.Allowlist(ctxWithToken("tok"), "u1") != nil {
		t.Error("nil client Allowlist should return nil")
	}
	if _, ok := nilc.ResolveDir(nil, "x"); ok {
		t.Error("nil client ResolveDir should miss")
	}
	if nilc.Dirs(nil) != nil {
		t.Error("nil client Dirs should return nil")
	}
}

// TestAllowlist_SuccessParse verifies a 200 allowlist parses into entries
// (name/description/path), the caller's JWT rides Authorization verbatim, and
// the role field is ignored.
func TestAllowlist_SuccessParse(t *testing.T) {
	var gotAuth string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"skills": []map[string]string{
				{
					"slug":        "s",
					"name":        "fund-promo-sentiment-v2",
					"description": "Fund promo sentiment analysis",
					"role":        "installed",
					"path":        "/019fef90-ee91-702f-b85e-e8f71973af44/fund-promo-sentiment-v2",
				},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got := c.Allowlist(ctxWithToken("jwt-abc"), "u1")
	if len(got) != 1 {
		t.Fatalf("allowlist = %+v, want 1 entry", got)
	}
	if got[0].Name != "fund-promo-sentiment-v2" || got[0].Description != "Fund promo sentiment analysis" {
		t.Errorf("entry = %+v", got[0])
	}
	if gotAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization = %q, want the raw login JWT", gotAuth)
	}
	if gotQuery != "" {
		t.Errorf("expected no query parameters, got %q", gotQuery)
	}
}

// TestAllowlist_FailClosedDegradation verifies every failure shape — 401,
// 403, 500, timeout, bad JSON, connection refused — degrades to a nil
// allowlist without erroring the call, and does not poison the cache (the
// next successful fetch works).
func TestAllowlist_FailClosedDegradation(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		delay  time.Duration
	}{
		{"401", http.StatusUnauthorized, `{"skills":[]}`, 0},
		{"403", http.StatusForbidden, `{"skills":[]}`, 0},
		{"500", http.StatusInternalServerError, "boom", 0},
		{"bad json", http.StatusOK, `{"skills":[`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.delay > 0 {
					time.Sleep(tc.delay)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestClient(t, srv.URL)
			if got := c.Allowlist(ctxWithToken("tok"), "u1"); got != nil {
				t.Errorf("allowlist = %+v, want nil (fail-closed)", got)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(500 * time.Millisecond)
			w.Write([]byte(`{"skills":[]}`))
		}))
		defer srv.Close()
		cfg := config.SkillMarketConfig{URL: srv.URL, Timeout: 50 * time.Millisecond, CacheTTL: time.Minute}
		c := New(cfg, "/m")
		start := time.Now()
		if got := c.Allowlist(ctxWithToken("tok"), "u1"); got != nil {
			t.Errorf("allowlist = %+v, want nil (fail-closed on timeout)", got)
		}
		if elapsed := time.Since(start); elapsed > 450*time.Millisecond {
			t.Errorf("fetch took %s; the per-call deadline did not bound it", elapsed)
		}
	})

	t.Run("unreachable host", func(t *testing.T) {
		c := newTestClient(t, "http://127.0.0.1:1")
		if got := c.Allowlist(ctxWithToken("tok"), "u1"); got != nil {
			t.Errorf("allowlist = %+v, want nil (fail-closed)", got)
		}
	})

	t.Run("failure not cached", func(t *testing.T) {
		var fail atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"skills":[{"name":"a","path":"/uid/a"}]}`))
		}))
		defer srv.Close()
		c := newTestClient(t, srv.URL)
		if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
			t.Fatalf("first fetch = %+v, want 1 entry", got)
		}
		fail.Store(true)
		if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
			t.Fatalf("TTL hit after failure-state = %+v, want the cached entry", got)
		}
		// New user (cache miss) during the outage gets nil, then recovers.
		if got := c.Allowlist(ctxWithToken("tok"), "u2"); got != nil {
			t.Fatalf("u2 during outage = %+v, want nil", got)
		}
		fail.Store(false)
		if got := c.Allowlist(ctxWithToken("tok"), "u2"); len(got) != 1 {
			t.Fatalf("u2 after recovery = %+v, want 1 entry (failures must not be cached)", got)
		}
	})
}

// TestAllowlist_EmptyTokenNoOutbound verifies a token-less context never
// issues an outbound request (an anonymous fetch could not be authorized
// anyway) and degrades fail-closed.
func TestAllowlist_EmptyTokenNoOutbound(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"skills":[]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	if got := c.Allowlist(context.Background(), "u1"); got != nil {
		t.Errorf("allowlist = %+v, want nil without a token", got)
	}
	if hits.Load() != 0 {
		t.Errorf("outbound requests = %d, want 0", hits.Load())
	}
	// An empty userID is likewise a no-op even with a token.
	if got := c.Allowlist(ctxWithToken("tok"), ""); got != nil {
		t.Errorf("allowlist = %+v, want nil without a userID", got)
	}
}

// TestAllowlist_TTLWindowSingleOutbound verifies the per-user cache: within
// the TTL two calls hit the market once, and after expiry they refetch. Two
// users never share a cache slot.
func TestAllowlist_TTLWindowSingleOutbound(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"skills":[{"name":"a","path":"/uid/a"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	base := time.Now()
	current := base
	c.now = func() time.Time { return current }

	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("first fetch = %+v", got)
	}
	current = base.Add(30 * time.Second) // inside the 60s window
	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("cached fetch = %+v", got)
	}
	if hits.Load() != 1 {
		t.Errorf("outbound requests within TTL = %d, want 1", hits.Load())
	}

	current = base.Add(61 * time.Second) // past the window
	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("post-expiry fetch = %+v", got)
	}
	if hits.Load() != 2 {
		t.Errorf("outbound requests after expiry = %d, want 2", hits.Load())
	}

	// Cached results are copies: caller mutation must not poison the cache.
	c.Allowlist(ctxWithToken("tok"), "u1") // warm (hit from the refetch window)
	c.mu.Lock()
	cached := c.cache["u1"].entries
	c.mu.Unlock()
	mutated := c.Allowlist(ctxWithToken("tok"), "u1")
	if len(mutated) > 0 {
		mutated[0].Name = "poisoned"
	}
	c.mu.Lock()
	still := c.cache["u1"].entries
	c.mu.Unlock()
	if len(still) > 0 && still[0].Name != cached[0].Name {
		t.Errorf("caller mutation leaked into the cache: %+v vs %+v", still, cached)
	}
}

// TestAllowlist_PerUserIsolation verifies two users fetch independently (the
// market server resolves identity from the JWT, not from any parameter).
func TestAllowlist_PerUserIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer jwt-a" {
			w.Write([]byte(`{"skills":[{"name":"for-a","path":"/uidA/for-a"}]}`))
			return
		}
		w.Write([]byte(`{"skills":[{"name":"for-b","path":"/uidB/for-b"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	if got := c.Allowlist(ctxWithToken("jwt-a"), "u1"); len(got) != 1 || got[0].Name != "for-a" {
		t.Fatalf("u1 = %+v", got)
	}
	if got := c.Allowlist(ctxWithToken("jwt-b"), "u2"); len(got) != 1 || got[0].Name != "for-b" {
		t.Fatalf("u2 = %+v", got)
	}
}

// TestResolveDir_NormalJoin verifies the happy-path join: the API path lands
// under the market root with the market-side user_id segment preserved.
func TestResolveDir_NormalJoin(t *testing.T) {
	c := newTestClient(t, "http://unused")
	allow := []Entry{{Name: "fund-promo-sentiment-v2", Path: "/019fef90-ee91-702f-b85e-e8f71973af44/fund-promo-sentiment-v2"}}
	dir, ok := c.ResolveDir(allow, "fund-promo-sentiment-v2")
	if !ok {
		t.Fatal("ResolveDir missed an allowlisted name")
	}
	want := "/data/skills-market/019fef90-ee91-702f-b85e-e8f71973af44/fund-promo-sentiment-v2"
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if _, ok := c.ResolveDir(allow, "other"); ok {
		t.Error("ResolveDir should miss a name not in the allowlist")
	}
}

// TestResolveDir_EscapeRejected verifies traversal attempts in the API path
// are dropped: no resolution, no read, no bind — the path is semi-trusted.
func TestResolveDir_EscapeRejected(t *testing.T) {
	c := newTestClient(t, "http://unused")
	cases := []struct {
		name string
		path string
	}{
		{"dotdot absolute", "/../../etc"},
		{"dotdot deep", "/uid/../../etc/passwd"},
		{"escape via suffix", "/uid/skill/../../../tmp"},
		{"empty", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allow := []Entry{{Name: "evil", Path: tc.path}}
			if dir, ok := c.ResolveDir(allow, "evil"); ok {
				t.Errorf("ResolveDir(%q) = %q, want rejection", tc.path, dir)
			}
		})
	}
	// A name that is not a simple identifier never resolves even if listed.
	allow := []Entry{{Name: "a/b", Path: "/uid/a"}}
	if _, ok := c.ResolveDir(allow, "a/b"); ok {
		t.Error("ResolveDir should reject a path-shaped name")
	}
}

// TestDirs_FlattenAndGuards verifies the sandbox-facing resolution: order
// preserved, duplicate names collapse to the first, escaping paths and
// non-identifier names drop, and the market root itself can never be a
// resolution target of an escaping entry.
func TestDirs_FlattenAndGuards(t *testing.T) {
	c := newTestClient(t, "http://unused")
	allow := []Entry{
		{Name: "first", Path: "/uid1/first"},
		{Name: "first", Path: "/uid2/first-dup"},
		{Name: "evil", Path: "/../escape"},
		{Name: "bad/name", Path: "/uid/x"},
		{Name: "second", Path: "/uid1/nested/second"},
	}
	dirs := c.Dirs(allow)
	want := []NamedDir{
		{Name: "first", Dir: "/data/skills-market/uid1/first"},
		{Name: "second", Dir: "/data/skills-market/uid1/nested/second"},
	}
	if len(dirs) != len(want) {
		t.Fatalf("Dirs = %+v, want %+v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("Dirs[%d] = %+v, want %+v", i, dirs[i], want[i])
		}
	}
}

// TestTokenRoundTrip verifies the ctx pair round-trips and that an empty
// token leaves the context untouched.
func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	if tok := TokenFromContext(ctx); tok != "" {
		t.Errorf("TokenFromContext(blank) = %q, want empty", tok)
	}
	ctx = WithToken(ctx, "tok-1")
	if tok := TokenFromContext(ctx); tok != "tok-1" {
		t.Errorf("TokenFromContext = %q, want tok-1", tok)
	}
	if TokenFromContext(WithToken(ctx, "")) != "tok-1" {
		t.Error("WithToken(empty) must not overwrite an existing value")
	}
}
