package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/artifact"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/model"
)

// artifactFakeIndex is an in-memory artifact.Index for handler tests.
type artifactFakeIndex struct {
	recs []model.FileVersion
}

func (f *artifactFakeIndex) InsertVersion(_ context.Context, rec model.FileVersion) error {
	rec.CreateTime = time.Now()
	f.recs = append(f.recs, rec)
	return nil
}

func (f *artifactFakeIndex) LatestVersion(_ context.Context, userID, path string) (*model.FileVersion, error) {
	return f.asOf(userID, path, time.Time{}), nil
}

func (f *artifactFakeIndex) VersionAsOf(_ context.Context, userID, path string, before time.Time) (*model.FileVersion, error) {
	return f.asOf(userID, path, before), nil
}

func (f *artifactFakeIndex) asOf(userID, path string, before time.Time) *model.FileVersion {
	var best *model.FileVersion
	for i := range f.recs {
		r := &f.recs[i]
		if r.UserID != userID || r.Path != path {
			continue
		}
		if !before.IsZero() && r.CreateTime.After(before) {
			continue
		}
		if best == nil || r.CreateTime.After(best.CreateTime) {
			best = r
		}
	}
	return best
}

func (f *artifactFakeIndex) VersionByID(_ context.Context, vid string) (*model.FileVersion, error) {
	for i := range f.recs {
		if f.recs[i].VersionID == vid {
			return &f.recs[i], nil
		}
	}
	return nil, nil
}

type artifactTestEnv struct {
	engine *gin.Engine
	svc    *artifact.Service
	idx    *artifactFakeIndex
	wsRoot string
}

func newArtifactTestEnv(t *testing.T, ooSecret string) *artifactTestEnv {
	t.Helper()
	blobs, err := artifact.NewBlobStore(t.TempDir())
	require.NoError(t, err)
	idx := &artifactFakeIndex{}
	svc := artifact.NewService(blobs, idx, 0)
	wsRoot := t.TempDir()

	h := NewArtifactHandler(svc, OnlyOfficeSettings{
		Secret:          ooSecret,
		ServerURL:       "http://oo.local",
		InternalBackend: "http://backend.local",
	})

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-art")
		c.Next()
	})
	r.GET("/api/v1/workspace/versions/resolve", h.Resolve)
	r.GET("/api/v1/workspace/versions/:vid/content", h.VersionContent)
	r.GET("/api/v1/workspace/versions/:vid/onlyoffice-config", h.VersionOnlyOfficeConfig)
	return &artifactTestEnv{engine: r, svc: svc, idx: idx, wsRoot: wsRoot}
}

func (e *artifactTestEnv) get(t *testing.T, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	return w
}

// seed writes a workspace file and finalizes a fake turn over it.
func (e *artifactTestEnv) seed(t *testing.T, rel, content string) artifact.Artifact {
	t.Helper()
	writeWorkspaceFile(t, e.wsRoot, rel, content)
	arts, err := e.svc.FinalizeTurn(context.Background(), "user-1", e.wsRoot, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	return arts[0]
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := root + "/" + rel
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func TestArtifactResolve_Latest(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	a := env.seed(t, "reports/a.md", "v1")

	w := env.get(t, "/api/v1/workspace/versions/resolve?path=reports/a.md")
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, a.VersionID, body["version_id"])
	assert.Equal(t, "reports/a.md", body["path"])
}

func TestArtifactResolve_MissingPathAndNeverVersioned(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	assert.Equal(t, http.StatusBadRequest, env.get(t, "/api/v1/workspace/versions/resolve").Code)
	assert.Equal(t, http.StatusNotFound, env.get(t, "/api/v1/workspace/versions/resolve?path=nope.md").Code)
}

func TestArtifactResolve_RejectsTraversal(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	assert.Equal(t, http.StatusForbidden, env.get(t, "/api/v1/workspace/versions/resolve?path=../x").Code)
	assert.Equal(t, http.StatusForbidden, env.get(t, "/api/v1/workspace/versions/resolve?path=/abs").Code)
}

func TestArtifactResolve_BadBefore(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	assert.Equal(t, http.StatusBadRequest, env.get(t, "/api/v1/workspace/versions/resolve?path=a.md&before=not-a-time").Code)
}

func TestArtifactVersionContent_ServesBytes(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	a := env.seed(t, "notes/hello.txt", "hello-version")

	w := env.get(t, "/api/v1/workspace/versions/"+a.VersionID+"/content")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hello-version", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")
}

func TestArtifactVersionContent_UnknownAndCrossUser404(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	a := env.seed(t, "a.txt", "mine")

	assert.Equal(t, http.StatusNotFound, env.get(t, "/api/v1/workspace/versions/does-not-exist/content").Code)

	// Cross-user: move ownership of the record; the endpoint must 404.
	env.idx.recs[0].UserID = "user-2"
	assert.Equal(t, http.StatusNotFound, env.get(t, "/api/v1/workspace/versions/"+a.VersionID+"/content").Code)
}

func TestArtifactVersionOnlyOfficeConfig_DisabledWithoutSecret(t *testing.T) {
	env := newArtifactTestEnv(t, "")
	a := env.seed(t, "doc.xlsx", "x")
	w := env.get(t, "/api/v1/workspace/versions/"+a.VersionID+"/onlyoffice-config")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestArtifactVersionOnlyOfficeConfig_ViewConfig(t *testing.T) {
	env := newArtifactTestEnv(t, "oo-secret")
	a := env.seed(t, "doc.xlsx", "x")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/versions/"+a.VersionID+"/onlyoffice-config", nil)
	req.Header.Set("Authorization", "Bearer jwt-123")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		ServerURL string `json:"server_url"`
		View      struct {
			Config map[string]any `json:"config"`
			Token  string         `json:"token"`
		} `json:"view"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "http://oo.local", body.ServerURL)
	assert.NotEmpty(t, body.View.Token)

	doc := body.View.Config["document"].(map[string]any)
	assert.Equal(t, "xlsx", doc["fileType"])
	docURL := doc["url"].(string)
	assert.Contains(t, docURL, "http://backend.local/api/v1/workspace/versions/"+a.VersionID+"/content?token=jwt-123")
	editor := body.View.Config["editorConfig"].(map[string]any)
	assert.Equal(t, "view", editor["mode"])
	_, hasCallback := editor["callbackUrl"]
	assert.False(t, hasCallback, "immutable version must not carry a save callback")

	// Cross-user: 404.
	env.idx.recs[0].UserID = "user-2"
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/versions/"+a.VersionID+"/onlyoffice-config", nil)
	req2.Header.Set("Authorization", "Bearer jwt-123")
	w2 := httptest.NewRecorder()
	env.engine.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Code)
}
