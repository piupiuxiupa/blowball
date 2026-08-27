package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/skillmarket"
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
	require.NoError(t, os.MkdirAll(fsSvc.UserSkills("user-1"), 0o755))

	h := NewSkillHandler(fsSvc, nil)
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
	return e.fsSvc.UserSkills("user-1")
}

// TestSkills_List verifies that skill files are listed with the correct shape:
// name (without extension), filename (with extension), size, update_time.
func TestSkills_List(t *testing.T) {
	env := newSkillTestEnv(t)
	dir := env.skillsDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "coder"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder", "SKILL.md"), []byte("You are a coding assistant."), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "review"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "review", "SKILL.md"), []byte("role: reviewer"), 0o644))

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
	assert.Equal(t, "coder", resp.Skills[0].Filename)
	assert.Greater(t, resp.Skills[0].Size, int64(0))
	assert.Equal(t, "review", resp.Skills[1].Name)
	assert.Equal(t, "review", resp.Skills[1].Filename)
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

// --- skill-market merge (skill-market capability) ---

// skillWireEntry mirrors the response entry shape for decode-based assertions.
type skillWireEntry struct {
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	UpdateTime  string `json:"update_time"`
	Location    string `json:"location"`
	Description string `json:"description"`
}

// newSkillMarketEnv builds a SkillHandler wired to an httptest market service
// that authenticates Bearer jwt-market and returns the supplied allowlist
// JSON. The engine's auth shim forwards the request's Authorization header so
// the handler's pass-through token flow is exercised end-to-end.
func newSkillMarketEnv(t *testing.T, allowlist string, status int) (*skillTestEnv, *httptest.Server) {
	t.Helper()
	env := newSkillTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-market" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(allowlist))
	}))
	t.Cleanup(srv.Close)

	cfg := config.SkillMarketConfig{URL: srv.URL, Timeout: 2 * time.Second, CacheTTL: time.Minute}
	env.handler.market = skillmarket.New(cfg, t.TempDir())
	require.NotNil(t, env.handler.market)
	return env, srv
}

func doGetSkills(t *testing.T, env *skillTestEnv, token string) ([]skillWireEntry, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	var body struct {
		Skills []skillWireEntry `json:"skills"`
	}
	if w.Code == http.StatusOK {
		require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	}
	return body.Skills, w.Code
}

// TestSkills_MarketMerge verifies market entries are appended with
// location "skill_market" and the API description, while a local skill with
// the same name wins the conflict.
func TestSkills_MarketMerge(t *testing.T) {
	env, _ := newSkillMarketEnv(t, `{"skills":[
		{"slug":"a","name":"fund-promo-sentiment-v2","description":"Market desc","role":"installed","path":"/uid/fund-promo-sentiment-v2"},
		{"slug":"b","name":"coder","description":"market coder","role":"installed","path":"/uid/coder"}
	]}`, 0)
	dir := env.skillsDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "coder"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder", "SKILL.md"), []byte("local coder"), 0o644))

	skills, code := doGetSkills(t, env, "jwt-market")
	require.Equal(t, http.StatusOK, code)
	byName := map[string]skillWireEntry{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	mkt := byName["fund-promo-sentiment-v2"]
	assert.Equal(t, skillmarket.LocationSkillMarket, mkt.Location)
	assert.Equal(t, "Market desc", mkt.Description)
	assert.Equal(t, "fund-promo-sentiment-v2", mkt.Filename)
	loc := byName["coder"]
	assert.Equal(t, "", loc.Location, "local entry keeps the pre-capability shape (no location key)")
	assert.Equal(t, "", loc.Description)
	assert.NotEqual(t, "market coder", loc.Description)
}

// TestSkills_MarketFailureDegradesToLocalOnly verifies a failing market
// (non-200) degrades fail-closed: HTTP 200 with only the local list.
func TestSkills_MarketFailureDegradesToLocalOnly(t *testing.T) {
	env, _ := newSkillMarketEnv(t, `{}`, http.StatusServiceUnavailable)
	dir := env.skillsDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "coder"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder", "SKILL.md"), []byte("x"), 0o644))

	skills, code := doGetSkills(t, env, "jwt-market")
	require.Equal(t, http.StatusOK, code, "market failure must not fail the endpoint")
	require.Len(t, skills, 1)
	assert.Equal(t, "coder", skills[0].Name)
}

// TestSkills_MarketNoTokenSkipped verifies a request without the Bearer
// header still returns 200 local-only (the fetch is skipped, not errored).
func TestSkills_MarketNoTokenSkipped(t *testing.T) {
	env, _ := newSkillMarketEnv(t, `{"skills":[{"name":"x","description":"d","path":"/uid/x"}]}`, 0)
	skills, code := doGetSkills(t, env, "")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, skills)
}

// TestSkills_MarketNilIdenticalShape verifies the nil-client wiring keeps the
// legacy response bytes: no location/description keys on local entries.
func TestSkills_MarketNilIdenticalShape(t *testing.T) {
	env := newSkillTestEnv(t)
	dir := env.skillsDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "coder"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder", "SKILL.md"), []byte("x"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "location", "local-only response must not carry the new keys")
	assert.NotContains(t, w.Body.String(), "description")
}
