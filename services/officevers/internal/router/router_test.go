package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	miniostore "github.com/lush/blowball/services/officevers/internal/minio"

	"github.com/gin-gonic/gin"
)

// stubStore is a minimal handler.Storage used only to build the router without a
// real MinIO, so we can assert route wiring and that the full engine (including
// /healthz) builds and resolves without panicking.
type stubStore struct{}

func (stubStore) Upload(context.Context, string, string, io.Reader, int64) (string, time.Time, error) {
	return "v", time.Time{}, nil
}
func (stubStore) ListVersions(context.Context, string, string) ([]miniostore.Version, error) {
	return nil, nil
}
func (stubStore) DownloadLatest(context.Context, string, string) (*miniostore.Download, error) {
	return &miniostore.Download{Reader: io.NopCloser(strings.NewReader("ok")), ContentType: "text/plain"}, nil
}
func (stubStore) DownloadVersion(context.Context, string, string, string) (*miniostore.Download, error) {
	return nil, miniostore.ErrNotFound
}
func (stubStore) ListDirectories(context.Context) ([]string, error)                { return []string{}, nil }
func (stubStore) Delete(context.Context, string, string) error                     { return nil }
func (stubStore) Rollback(context.Context, string, string, string) (string, error) {
	return "v", nil
}

func TestRouterBuilds(t *testing.T) {
	r := New(stubStore{})
	routes := r.Routes()
	if len(routes) == 0 {
		t.Fatal("no routes registered")
	}
	// Ensure all expected paths are registered.
	want := map[string]bool{
		"GET /documents":                    true,
		"GET /documents/:uuid/*filepath":    true,
		"POST /documents/:uuid/*filepath":   true,
		"DELETE /documents/:uuid/*filepath": true,
		"GET /healthz":                      true,
	}
	got := map[string]bool{}
	for _, rt := range routes {
		got[rt.Method+" "+rt.Path] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing route %q; registered: %v", k, keys(got))
		}
	}
}

func TestHealthz(t *testing.T) {
	r := New(stubStore{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("healthz body = %s", w.Body.String())
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Ensure gin is imported (used indirectly via httptest/engine). This keeps the
// import list honest if the file is trimmed later.
var _ = gin.Mode
