package mcpmarket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/skillmarket"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c := New(config.MCPMarketConfig{URL: url, Timeout: 2 * time.Second, CacheTTL: time.Minute}, "/data/mcp-market")
	if c == nil {
		t.Fatal("New returned nil for an enabled config")
	}
	return c
}

func ctxWithToken(token string) context.Context {
	return skillmarket.WithToken(context.Background(), token)
}

func TestNew_DisabledReturnsNil(t *testing.T) {
	if c := New(config.MCPMarketConfig{URL: "", Timeout: time.Second, CacheTTL: time.Minute}, "/m"); c != nil {
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

func TestAllowlist_SuccessParseAndJWT(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"servers":[{"slug":"s","name":"github-market","description":"GitHub MCP","path":"/019fef90-…/github-market"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got := c.Allowlist(ctxWithToken("jwt-abc"), "u1")
	if len(got) != 1 || got[0].Name != "github-market" || got[0].Path != "/019fef90-…/github-market" {
		t.Fatalf("allowlist = %+v, want 1 github-market entry", got)
	}
	if gotAuth != "Bearer jwt-abc" {
		t.Errorf("Authorization = %q, want the raw login JWT", gotAuth)
	}
}

func TestAllowlist_FailClosedDegradation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, `{"servers":[]}`},
		{"403", http.StatusForbidden, `{"servers":[]}`},
		{"500", http.StatusInternalServerError, "boom"},
		{"bad json", http.StatusOK, `{"servers":[`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if got := newTestClient(t, srv.URL).Allowlist(ctxWithToken("tok"), "u1"); got != nil {
				t.Errorf("allowlist = %+v, want nil (fail-closed)", got)
			}
		})
	}
	t.Run("unreachable host", func(t *testing.T) {
		if got := newTestClient(t, "http://127.0.0.1:1").Allowlist(ctxWithToken("tok"), "u1"); got != nil {
			t.Errorf("allowlist = %+v, want nil (fail-closed)", got)
		}
	})
}

func TestAllowlist_EmptyTokenNoOutbound(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"servers":[]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	if got := c.Allowlist(context.Background(), "u1"); got != nil {
		t.Errorf("allowlist = %+v, want nil without a token", got)
	}
	if hits.Load() != 0 {
		t.Errorf("outbound requests = %d, want 0", hits.Load())
	}
	if got := c.Allowlist(ctxWithToken("tok"), ""); got != nil {
		t.Errorf("allowlist = %+v, want nil without a userID", got)
	}
}

func TestAllowlist_TTLWindowSingleOutbound(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"servers":[{"name":"a","path":"/uid/a"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	base := time.Now()
	current := base
	c.now = func() time.Time { return current }

	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("first fetch = %+v", got)
	}
	current = base.Add(30 * time.Second)
	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("cached fetch = %+v", got)
	}
	if hits.Load() != 1 {
		t.Errorf("outbound requests within TTL = %d, want 1", hits.Load())
	}
	current = base.Add(61 * time.Second)
	if got := c.Allowlist(ctxWithToken("tok"), "u1"); len(got) != 1 {
		t.Fatalf("post-expiry fetch = %+v", got)
	}
	if hits.Load() != 2 {
		t.Errorf("outbound requests after expiry = %d, want 2", hits.Load())
	}
}

func TestAllowlist_PerUserIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer jwt-a" {
			w.Write([]byte(`{"servers":[{"name":"for-a","path":"/uidA/for-a"}]}`))
			return
		}
		w.Write([]byte(`{"servers":[{"name":"for-b","path":"/uidB/for-b"}]}`))
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

func TestResolveDir_JoinAndEscape(t *testing.T) {
	c := newTestClient(t, "http://unused")
	allow := []Entry{{Name: "github-market", Path: "/019fef90-…/github-market"}}
	dir, ok := c.ResolveDir(allow, "github-market")
	if !ok || dir != "/data/mcp-market/019fef90-…/github-market" {
		t.Fatalf("ResolveDir = %q,%v", dir, ok)
	}
	if _, ok := c.ResolveDir(allow, "other"); ok {
		t.Error("ResolveDir should miss a name not in the allowlist")
	}
	for _, path := range []string{"/../../etc", "/uid/../../etc/passwd", ""} {
		if dir, ok := c.ResolveDir([]Entry{{Name: "evil", Path: path}}, "evil"); ok {
			t.Errorf("ResolveDir(%q) = %q, want rejection", path, dir)
		}
	}
	if _, ok := c.ResolveDir([]Entry{{Name: "a/b", Path: "/uid/a"}}, "a/b"); ok {
		t.Error("ResolveDir should reject a path-shaped name")
	}
}
