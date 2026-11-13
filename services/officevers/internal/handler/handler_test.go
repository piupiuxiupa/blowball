package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	miniostore "github.com/lush/blowball/services/officevers/internal/minio"

	"github.com/gin-gonic/gin"
)

// mockStore is an in-memory handler.Storage used to exercise the HTTP layer
// without a real MinIO instance.
type mockStore struct {
	uploadVersionID string
	uploadErr       error
	listVersions    []miniostore.Version
	listVersionsErr error
	download        *miniostore.Download
	downloadErr     error
	listDirs        []string
	listDirsErr     error
	deleteErr       error
	rollbackVid     string
	rollbackErr     error

	// captured inputs for assertions
	gotUUID        string
	gotPath        string
	gotVersionID   string
	gotReader      io.Reader
	gotSize        int64
	rollbackCalled bool
	deleteCalled   bool
	uploadCalled   bool
}

func (m *mockStore) Upload(_ context.Context, uuid, path string, reader io.Reader, size int64) (string, time.Time, error) {
	m.uploadCalled, m.gotUUID, m.gotPath, m.gotReader, m.gotSize = true, uuid, path, reader, size
	if m.uploadErr != nil {
		return "", time.Time{}, m.uploadErr
	}
	return m.uploadVersionID, time.Unix(1700000000, 0).UTC(), nil
}

func (m *mockStore) ListVersions(_ context.Context, uuid, path string) ([]miniostore.Version, error) {
	m.gotUUID, m.gotPath = uuid, path
	return m.listVersions, m.listVersionsErr
}

func (m *mockStore) DownloadLatest(_ context.Context, uuid, path string) (*miniostore.Download, error) {
	m.gotUUID, m.gotPath, m.gotVersionID = uuid, path, ""
	return m.download, m.downloadErr
}

func (m *mockStore) DownloadVersion(_ context.Context, uuid, path, versionID string) (*miniostore.Download, error) {
	m.gotUUID, m.gotPath, m.gotVersionID = uuid, path, versionID
	return m.download, m.downloadErr
}

func (m *mockStore) ListDirectories(_ context.Context) ([]string, error) {
	return m.listDirs, m.listDirsErr
}

func (m *mockStore) Delete(_ context.Context, uuid, path string) error {
	m.deleteCalled, m.gotUUID, m.gotPath = true, uuid, path
	return m.deleteErr
}

func (m *mockStore) Rollback(_ context.Context, uuid, path, targetVersionID string) (string, error) {
	m.rollbackCalled, m.gotUUID, m.gotPath, m.gotVersionID = true, uuid, path, targetVersionID
	return m.rollbackVid, m.rollbackErr
}

func newTestEngine(store handlerImpl) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{Store: store}
	docs := r.Group("/documents")
	{
		docs.GET("", h.ListDirectories)
		docs.GET("/:uuid/*filepath", h.Document)
		docs.POST("/:uuid/*filepath", h.Document)
		docs.DELETE("/:uuid/*filepath", h.Document)
	}
	return r
}

// handlerImpl is the Storage interface type, named here to keep the mock
// declaration concise.
type handlerImpl = Storage

func do(t *testing.T, r *gin.Engine, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRoutesDoNotPanic(t *testing.T) {
	// Building the engine must not panic: this is the regression guard for the
	// catch-all + static "" route combination.
	r := newTestEngine(&mockStore{listDirs: []string{}})
	if len(r.Routes()) == 0 {
		t.Fatal("no routes registered")
	}
}

func TestUploadRoute(t *testing.T) {
	store := &mockStore{uploadVersionID: "v1"}
	r := newTestEngine(store)

	w := do(t, r, http.MethodPost, "/documents/abc/report.docx", strings.NewReader("hello"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
	if !store.uploadCalled {
		t.Fatal("Upload was not called")
	}
	if store.gotUUID != "abc" || store.gotPath != "report.docx" {
		t.Fatalf("got uuid=%q path=%q", store.gotUUID, store.gotPath)
	}
	if !strings.Contains(w.Body.String(), `"versionId":"v1"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestUploadLogicalPathWithDirectories(t *testing.T) {
	store := &mockStore{uploadVersionID: "v1"}
	r := newTestEngine(store)

	w := do(t, r, http.MethodPost, "/documents/abc/财务/2024/年报.docx", strings.NewReader("hello"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
	if store.gotPath != "财务/2024/年报.docx" {
		t.Fatalf("logical path = %q, want 财务/2024/年报.docx", store.gotPath)
	}
}

func TestDownloadLatestRoute(t *testing.T) {
	store := &mockStore{download: &miniostore.Download{
		Reader: io.NopCloser(strings.NewReader("latest-content")),
		VersionID: "v3", Size: int64(len("latest-content")), ContentType: "application/octet-stream",
	}}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/report.docx", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != "latest-content" {
		t.Fatalf("body = %q", w.Body.String())
	}
	if store.gotVersionID != "" {
		t.Fatalf("DownloadLatest should pass empty version id, got %q", store.gotVersionID)
	}
}

func TestDownloadVersionRoute(t *testing.T) {
	store := &mockStore{download: &miniostore.Download{
		Reader: io.NopCloser(strings.NewReader("v2-content")), VersionID: "v2",
		Size: 10, ContentType: "application/octet-stream",
	}}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/report.docx?action=version&versionId=v2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if store.gotVersionID != "v2" {
		t.Fatalf("expected DownloadVersion(v2), got %q", store.gotVersionID)
	}
}

func TestDownloadVersionMissingId(t *testing.T) {
	store := &mockStore{}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/report.docx?action=version", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestListVersionsRoute(t *testing.T) {
	store := &mockStore{listVersions: []miniostore.Version{
		{VersionID: "v3", IsLatest: true, Size: 3},
		{VersionID: "v2", Size: 2},
		{VersionID: "v1", Size: 1},
	}}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/report.docx?action=versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"isLatest":true`) {
		t.Fatalf("expected an isLatest entry; body=%s", w.Body.String())
	}
	if strings.Count(w.Body.String(), `"versionId"`) != 3 {
		t.Fatalf("expected 3 versions; body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"path":"report.docx"`) {
		t.Fatalf("expected path field in response; body=%s", w.Body.String())
	}
}

func TestDeleteRoute(t *testing.T) {
	store := &mockStore{}
	r := newTestEngine(store)

	w := do(t, r, http.MethodDelete, "/documents/abc/report.docx", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if !store.deleteCalled {
		t.Fatal("Delete was not called")
	}
}

func TestRollbackRoute(t *testing.T) {
	store := &mockStore{rollbackVid: "v4"}
	r := newTestEngine(store)

	body := bytes.NewBufferString(`{"versionId":"v1"}`)
	w := do(t, r, http.MethodPost, "/documents/abc/report.docx?action=rollback", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !store.rollbackCalled || store.gotVersionID != "v1" {
		t.Fatalf("rollback called=%v version=%q", store.rollbackCalled, store.gotVersionID)
	}
}

// TestLogicalPathWithReservedSegment proves the ?action= design removes the old
// reserved-name ambiguity: a directory literally named "versions" or "rollback"
// is treated as part of the logical path, not as an action suffix.
func TestLogicalPathWithReservedSegment(t *testing.T) {
	store := &mockStore{listVersions: []miniostore.Version{
		{VersionID: "v1", IsLatest: true},
	}}
	r := newTestEngine(store)

	// ".../versions/notes.docx" must be the logical path; ?action=versions is
	// the operation.
	w := do(t, r, http.MethodGet, "/documents/abc/reports/versions/notes.docx?action=versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if store.gotPath != "reports/versions/notes.docx" {
		t.Fatalf("logical path = %q, want reports/versions/notes.docx", store.gotPath)
	}

	// Same idea for "rollback" as a directory name.
	store2 := &mockStore{rollbackVid: "v9"}
	r2 := newTestEngine(store2)
	body := bytes.NewBufferString(`{"versionId":"v1"}`)
	w2 := do(t, r2, http.MethodPost, "/documents/abc/drafts/rollback/final.docx?action=rollback", body)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w2.Code, w2.Body.String())
	}
	if store2.gotPath != "drafts/rollback/final.docx" {
		t.Fatalf("logical path = %q, want drafts/rollback/final.docx", store2.gotPath)
	}
}

func TestUnsupportedAction(t *testing.T) {
	store := &mockStore{}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/report.docx?action=bogus", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestListDirectoriesRoute(t *testing.T) {
	store := &mockStore{listDirs: []string{"abc", "xyz"}}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "abc") || !strings.Contains(w.Body.String(), "xyz") {
		t.Fatalf("body missing directories: %s", w.Body.String())
	}
}

func TestErrorMappingNotFound(t *testing.T) {
	store := &mockStore{listVersionsErr: miniostore.ErrNotFound}
	r := newTestEngine(store)

	w := do(t, r, http.MethodGet, "/documents/abc/missing.docx?action=versions", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestErrorMappingValidation(t *testing.T) {
	store := &mockStore{uploadErr: miniostore.ErrInvalidPath}
	r := newTestEngine(store)

	w := do(t, r, http.MethodPost, "/documents/abc/report.docx", strings.NewReader("x"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestErrorMappingInternal(t *testing.T) {
	store := &mockStore{uploadErr: errors.New("boom")}
	r := newTestEngine(store)

	w := do(t, r, http.MethodPost, "/documents/abc/report.docx", strings.NewReader("x"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

func TestRollbackMissingVersionId(t *testing.T) {
	store := &mockStore{rollbackVid: "v4"}
	r := newTestEngine(store)

	w := do(t, r, http.MethodPost, "/documents/abc/report.docx?action=rollback", strings.NewReader(""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if store.rollbackCalled {
		t.Fatal("Rollback should not have been called")
	}
}
