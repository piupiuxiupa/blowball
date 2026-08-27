package integration

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

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/jwt"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/luban"
	"github.com/lush/blowball/internal/tool/skill"
)

// This file exercises the skill-market capability end-to-end against a fake
// market service that VERIFIES blowball login JWTs with the shared jwt.secret
// (the deployment contract): the identity comes from the token, the allowlist
// from the server, the payloads from the operator-synced market root.

// marketSkillSpec is one fake-market entry plus its disk-synced state.
type marketSkillSpec struct {
	name    string
	path    string // API path under the market root
	synced  bool   // whether the operator has landed the directory on disk
	visible string // "" = everyone; else only this userID sees it
}

// fakeMarket is an httptest skill-market service: it verifies the Bearer JWT
// with the shared secret, derives the user identity from the token (never from
// a parameter), and serves each skill to its authorized users only.
type fakeMarket struct {
	srv    *httptest.Server
	skills []marketSkillSpec
	secret string
	hits   int
}

func newFakeMarket(t *testing.T, secret string, skills ...marketSkillSpec) *fakeMarket {
	t.Helper()
	fm := &fakeMarket{skills: skills, secret: secret}
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fm.hits++
		token := r.Header.Get("Authorization")
		require.NotEmpty(t, token, "market request must carry the Authorization header")
		userID, err := jwt.Verify(secret, trimBearer(token))
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		out := []map[string]string{}
		for _, s := range fm.skills {
			if s.visible != "" && s.visible != userID {
				continue
			}
			out = append(out, map[string]string{
				"slug":        s.name,
				"name":        s.name,
				"description": "market description of " + s.name,
				"role":        "installed",
				"path":        s.path,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"skills": out})
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

func trimBearer(h string) string {
	if len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return h
}

// syncMarketRoot materializes the synced subset of specs under root and
// returns it.
func syncMarketRoot(t *testing.T, root string, specs []marketSkillSpec) string {
	t.Helper()
	for _, s := range specs {
		if !s.synced {
			continue
		}
		dir := filepath.Join(root, filepath.Clean(s.path))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+s.name+"\ndescription: disk\n---\nSynced body of "+s.name+".\n"), 0o644))
	}
	return root
}

// newMarketTestEnv wires a gin engine with GET /api/v1/skills behind real
// auth middleware, plus a luban tool registry, both sharing one market client
// over a fake market service and a synced market root.
type marketTestEnv struct {
	engine  *gin.Engine
	reg     *tool.Registry
	market  *skillmarket.Client
	fake    *fakeMarket
	root    string
	fsSvc   *fs.Store
	dataDir string
}

func newMarketTestEnv(t *testing.T, skills ...marketSkillSpec) *marketTestEnv {
	t.Helper()
	dataDir := t.TempDir()
	marketRoot := syncMarketRoot(t, filepath.Join(dataDir, "skills-market"), skills)
	fake := newFakeMarket(t, integrationTestSecret, skills...)

	cfg := config.SkillMarketConfig{URL: fake.srv.URL, Timeout: 2 * time.Second, CacheTTL: time.Minute}
	market := skillmarket.New(cfg, marketRoot)
	require.NotNil(t, market)

	fsSvc, err := fs.New(dataDir)
	require.NoError(t, err)

	// luban tools registered exactly as wireAgent does, market attached.
	loader := skill.NewLoader(filepath.Join(dataDir, "skills"), fsSvc.UserSkills)
	lubanTools := luban.NewTools(loader, fsSvc.UserSkills).WithMarket(market)
	reg := tool.NewRegistry()
	require.NoError(t, luban.RegisterAll(reg, lubanTools))

	skillH := handler.NewSkillHandler(fsSvc, market)
	engine := gin.New()
	engine.Use(middleware.TraceMiddleware())
	authed := engine.Group("/api/v1", middleware.AuthMiddleware(integrationTestSecret))
	authed.GET("/skills", skillH.List)

	return &marketTestEnv{engine: engine, reg: reg, market: market, fake: fake, root: marketRoot, fsSvc: fsSvc, dataDir: dataDir}
}

// signedLogin mints the login JWT the way the auth service does.
func signedLogin(t *testing.T, userID string) string {
	t.Helper()
	tok, err := jwt.Sign(integrationTestSecret, userID, time.Hour)
	require.NoError(t, err)
	return tok
}

// turnCtx reproduces the tool-execution context a real turn provides: the
// skill userID plus the raw login token injected by MessageStreamHandler.
func turnCtx(t *testing.T, userID string) context.Context {
	t.Helper()
	return skillmarket.WithToken(skill.WithUserID(context.Background(), userID), signedLogin(t, userID))
}

func callTool[Q any](t *testing.T, env *marketTestEnv, ctx context.Context, name string, args Q) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := env.reg.Call(ctx, name, raw)
	require.NoError(t, err, "tool %s failed: %v", name, err)
	// Round-trip through JSON so cross-package assertions work against the
	// unexported luban result types.
	encoded, err := json.Marshal(res)
	require.NoError(t, err)
	return encoded
}

// TestSkillMarket_LubanListMergeAndReadThirdSource covers the agent path
// through the real registry: the list merges market entries (local-first on
// conflicts), and luban_read_skill resolves a market skill as the third
// source with a disk-lagged entry erroring explicitly.
func TestSkillMarket_LubanListMergeAndReadThirdSource(t *testing.T) {
	env := newMarketTestEnv(t,
		marketSkillSpec{name: "fund-promo", path: "/uid-1/fund-promo", synced: true},
		marketSkillSpec{name: "lagging", path: "/uid-1/lagging", synced: false},
	)
	// A LOCAL skill shadowing a market entry with the same name.
	localDir := env.fsSvc.UserSkills(defaultUserID)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "local-wins"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local-wins", "SKILL.md"),
		[]byte("---\nname: local-wins\ndescription: local\n---\nLOCAL body.\n"), 0o644))
	env.fake.skills = append(env.fake.skills, marketSkillSpec{name: "local-wins", path: "/uid-2/local-wins", synced: true})

	ctx := turnCtx(t, defaultUserID)

	// 1. List merge.
	var list []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Location    string `json:"location"`
	}
	require.NoError(t, json.Unmarshal(callTool(t, env, ctx, luban.ToolListSkills, struct{}{}), &list))
	byName := map[string][2]string{}
	for _, e := range list {
		byName[e.Name] = [2]string{e.Description, e.Location}
	}
	require.Contains(t, byName, "fund-promo")
	assert.Equal(t, "market description of fund-promo", byName["fund-promo"][0])
	assert.Equal(t, "skill_market", byName["fund-promo"][1])
	assert.Equal(t, "local", byName["local-wins"][0], "local skill wins the name conflict")
	assert.Equal(t, "user", byName["local-wins"][1])
	assert.Contains(t, byName, "lagging", "listed-but-unsynced still shows in the list")

	// 2. Read the third source (market hit, disk synced).
	var body string
	require.NoError(t, json.Unmarshal(callTool(t, env, ctx, luban.ToolReadSkill, map[string]string{"name": "fund-promo"}), &body))
	assert.Equal(t, "Synced body of fund-promo.", body)

	// 3. Local-first on the read path for the shadowed name.
	require.NoError(t, json.Unmarshal(callTool(t, env, ctx, luban.ToolReadSkill, map[string]string{"name": "local-wins"}), &body))
	assert.Equal(t, "LOCAL body.", body)

	// 4. Disk lag errors explicitly.
	_, err := env.reg.Call(ctx, luban.ToolReadSkill, mustJSON(t, map[string]string{"name": "lagging"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found")

	// 5. One allowlist fetch served list + reads (TTL cache): hits grew by 1.
	assert.Equal(t, 1, env.fake.hits, "TTL cache must collapse the turn's lookups into one outbound request")
}

// TestSkillMarket_SkillsEndpointMergeAndFailClosed covers the HTTP path:
// GET /api/v1/skills merges market entries labeled skill_market (local-first),
// a per-user allowlist difference is honored, and a dead market degrades to
// the local list with HTTP 200.
func TestSkillMarket_SkillsEndpointMergeAndFailClosed(t *testing.T) {
	env := newMarketTestEnv(t,
		marketSkillSpec{name: "fund-promo", path: "/uid-1/fund-promo", synced: true, visible: defaultUserID},
		marketSkillSpec{name: "other-user-skill", path: "/uid-2/other", synced: true, visible: "user-other"},
	)
	localDir := env.fsSvc.UserSkills(defaultUserID)
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, "coder"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "coder", "SKILL.md"), []byte("x"), 0o644))

	get := func(token string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	type entry struct {
		Name        string `json:"name"`
		Location    string `json:"location"`
		Description string `json:"description"`
	}
	decode := func(t *testing.T, b []byte) []entry {
		var body struct {
			Skills []entry `json:"skills"`
		}
		require.NoError(t, json.Unmarshal(b, &body))
		return body.Skills
	}

	// Merged, labeled, user-scoped.
	code, raw := get(signedLogin(t, defaultUserID))
	require.Equal(t, http.StatusOK, code)
	entries := decode(t, raw)
	names := map[string]entry{}
	for _, e := range entries {
		names[e.Name] = e
	}
	assert.Equal(t, "skill_market", names["fund-promo"].Location)
	assert.Equal(t, "market description of fund-promo", names["fund-promo"].Description)
	assert.NotContains(t, names, "other-user-skill", "another user's market skill must not appear")
	assert.Equal(t, "", names["coder"].Location, "local entries keep the legacy shape")

	// Fail-closed: the market dies. Within the TTL window the PREVIOUS user's
	// cached allowlist legitimately survives (the revoke-latency bound), but a
	// FRESH user's fetch fails closed — HTTP 200, local list only, no market
	// entries.
	env.fake.srv.Close()
	code, raw = get(signedLogin(t, defaultUserID))
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, string(raw), "fund-promo", "cached allowlist survives within the TTL window (documented revoke latency)")

	freshDir := env.fsSvc.UserSkills("user-fresh")
	require.NoError(t, os.MkdirAll(filepath.Join(freshDir, "solo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(freshDir, "solo", "SKILL.md"), []byte("x"), 0o644))
	code, raw = get(signedLogin(t, "user-fresh"))
	require.Equal(t, http.StatusOK, code, "a dead market must not fail the endpoint")
	entries = decode(t, raw)
	require.Len(t, entries, 1)
	assert.Equal(t, "solo", entries[0].Name)
}

// TestSkillMarket_ValidateSubPathSymlinkLayout pins the D4 open item: the
// shared skill.ValidateSubPath (whose EvalSymlinks both roots and targets)
// behaves correctly in mount/bind-like layouts — an alias (symlink) of the
// skill directory resolves as itself, an inside-the-tree symlink to another
// in-tree file resolves fine, and a symlink escaping the skill directory is
// rejected. This is the exact confinement luban applies to market skills.
func TestSkillMarket_ValidateSubPathSymlinkLayout(t *testing.T) {
	root := t.TempDir()
	// The "host" market skill directory.
	hostSkill := filepath.Join(root, "uid", "skill-a")
	require.NoError(t, os.MkdirAll(filepath.Join(hostSkill, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hostSkill, "SKILL.md"), []byte("m"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(hostSkill, "scripts", "run.py"), []byte("print(1)"), 0o644))

	// The in-sandbox alias: /skills/market/skill-a is a DIFFERENT path
	// (a bind mount in production, a symlink in this layout simulation).
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "skill-a")
	require.NoError(t, os.Symlink(hostSkill, alias))

	// A normal sub-path resolves inside the aliased root.
	p, err := skill.ValidateSubPath(alias, "scripts/run.py")
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "print(1)", string(data))

	// An in-tree symlink (scripts/alias.py → ../SKILL.md) is fine: it
	// resolves INSIDE the skill root.
	require.NoError(t, os.Symlink("../SKILL.md", filepath.Join(hostSkill, "scripts", "alias.py")))
	p, err = skill.ValidateSubPath(alias, "scripts/alias.py")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(p))

	// An escaping symlink (escape.md → ../../outside.txt) is rejected even
	// though the target exists.
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.Symlink("../../outside.txt", filepath.Join(hostSkill, "escape.md")))
	_, err = skill.ValidateSubPath(alias, "escape.md")
	require.Error(t, err, "a symlink escaping the skill directory must be rejected under an aliased root")

	// And traversal via .. is rejected identically.
	_, err = skill.ValidateSubPath(alias, "../../outside.txt")
	require.Error(t, err)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
