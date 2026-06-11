package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/store/fs"
)

// skillTestEnv sets up a temp data dir with a user skills directory and wires
// the SkillHandler into a gin engine.
type skillTestEnv struct {
	handler *SkillHandler
	engine  *gin.Engine
	dataDir string
	fsSvc   *fs.Store
}

func newSkillTestEnv(t *testing.T) *skillTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "user-1", "skills"), 0o755))

	h := NewSkillHandler(fsSvc)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "user-1")
		c.Set(middleware.TraceIDKey, "trace-skills")
		c.Next()
	})
	r.GET("/api/v1/skills", h.List)

	return &skillTestEnv{handler: h, engine: r, dataDir: dataDir, fsSvc: fsSvc}
}

func (e *skillTestEnv) skillsDir() string {
	return filepath.Join(e.dataDir, "user-1", "skills")
}

// TestSkills_List verifies that skill files are listed with the correct shape:
// name (without extension), filename (with extension), size, update_time.
func TestSkills_List(t *testing.T) {
	env := newSkillTestEnv(t)
	dir := env.skillsDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder.md"), []byte("You are a coding assistant."), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review.yaml"), []byte("role: reviewer"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Skills []struct {
			Name       string `json:"name"`
			Filename   string `json:"filename"`
			Size       int64  `json:"size"`
			UpdateTime string `json:"update_time"`
		} `json:"skills"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Skills, 2)

	// Sorted by name (coder < review).
	assert.Equal(t, "coder", resp.Skills[0].Name)
	assert.Equal(t, "coder.md", resp.Skills[0].Filename)
	assert.Greater(t, resp.Skills[0].Size, int64(0))
	assert.Equal(t, "review", resp.Skills[1].Name)
	assert.Equal(t, "review.yaml", resp.Skills[1].Filename)
}

// TestSkills_EmptyDir verifies an empty skills directory returns 200 with an
// empty JSON array.
func TestSkills_EmptyDir(t *testing.T) {
	env := newSkillTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Skills []any `json:"skills"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Skills)
}
