package luban

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/skill"
)

// marketFixture wires a luban-compatible skillmarket.Client against an
// httptest market service and a temp market root carrying one SYNCED skill
// (market-uid/market-skill) plus entries that are listed but not synced
// (disk lag). The token-carrying ctx mirrors what the streaming handler
// injects into tool execution.
type marketFixture struct {
	root   string
	client *skillmarket.Client
	srv    *httptest.Server
}

func newMarketFixture(t *testing.T) *marketFixture {
	t.Helper()
	root := t.TempDir()
	// One synced skill: {market_uid}/market-skill/SKILL.md + a text asset.
	skillDir := filepath.Join(root, "019fef90-ee91-702f-b85e-e8f71973af44", "market-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "examples"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: market-skill\ndescription: disk description\ndisk-path: true\n---\nMarket body.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "examples", "demo.py"),
		[]byte("print('market skill asset')\n"), 0o644))

	f := &marketFixture{root: root}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"skills": []map[string]string{
			{
				"slug":        "ms",
				"name":        "market-skill",
				"description": "market API description",
				"role":        "installed",
				"path":        "/019fef90-ee91-702f-b85e-e8f71973af44/market-skill",
			},
			{
				"slug":        "bar",
				"name":        "bar",
				"description": "listed but not synced",
				"role":        "installed",
				"path":        "/019fef90-ee91-702f-b85e-e8f71973af44/bar",
			},
			{
				"slug":        "f",
				"name":        "foo",
				"description": "market foo",
				"role":        "installed",
				"path":        "/019fef90-ee91-702f-b85e-e8f71973af44/foo",
			},
		}})
	}))
	f.srv = srv
	t.Cleanup(srv.Close)

	cfg := config.SkillMarketConfig{URL: srv.URL, Timeout: 2 * time.Second, CacheTTL: time.Minute}
	f.client = skillmarket.New(cfg, root)
	require.NotNil(t, f.client)
	return f
}

func marketCtx() context.Context {
	return skillmarket.WithToken(context.Background(), "jwt-test")
}

// TestListSkills_MarketMerge verifies the merged list: market entries carry
// location "skill_market" with the API description, and a name conflict with
// a LOCAL skill resolves local-first (the market entry disappears).
func TestListSkills_MarketMerge(t *testing.T) {
	f := newMarketFixture(t)
	globalDir := t.TempDir()
	userDataDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "foo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "foo", "SKILL.md"),
		[]byte("---\nname: foo\ndescription: local foo\n---\nlocal body\n"), 0o644))
	loader := skill.NewLoader(globalDir, func(userID string) string {
		return filepath.Join(userDataDir, userID, "skills")
	})

	entries, err := listSkills(marketCtx(), loader, f.client, "u1")
	require.NoError(t, err)
	byName := map[string]skillEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	assert.Equal(t, "local foo", byName["foo"].Description, "local skill wins the name conflict")
	assert.Equal(t, "global", byName["foo"].Location)
	assert.Equal(t, "market API description", byName["market-skill"].Description, "market description comes from the API")
	assert.Equal(t, skillmarket.LocationSkillMarket, byName["market-skill"].Location)
	assert.Contains(t, byName, "bar", "a listed-but-unsynced market skill still appears (API is the visibility truth)")
	// Sorted by name across sources.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	assert.IsNonDecreasing(t, names)
}

// TestListSkills_MarketFailureDegradesToLocalOnly verifies a dead market
// degrades fail-closed: the tool call succeeds with only local skills.
func TestListSkills_MarketFailureDegradesToLocalOnly(t *testing.T) {
	f := newMarketFixture(t)
	globalDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "foo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "foo", "SKILL.md"),
		[]byte("---\nname: foo\ndescription: local foo\n---\nlocal\n"), 0o644))
	loader := skill.NewLoader(globalDir, func(userID string) string { return t.TempDir() })

	f.srv.Close() // market down

	entries, err := listSkills(marketCtx(), loader, f.client, "u1")
	require.NoError(t, err, "a market failure must not fail the tool call")
	require.Len(t, entries, 1)
	assert.Equal(t, "foo", entries[0].Name)
}

// TestListSkills_MarketNilMatchesLegacy verifies the nil-dependency wiring is
// behaviorally identical to the pre-capability list: same entries, same order.
func TestListSkills_MarketNilMatchesLegacy(t *testing.T) {
	globalDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "foo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "foo", "SKILL.md"),
		[]byte("---\nname: foo\ndescription: d\n---\nb\n"), 0o644))
	loader := skill.NewLoader(globalDir, func(userID string) string { return t.TempDir() })

	withNil, err := listSkills(marketCtx(), loader, nil, "u1")
	require.NoError(t, err)

	legacy := loader.List("u1")
	want := make([]skillEntry, 0, len(legacy))
	for _, s := range legacy {
		want = append(want, skillEntry{Name: s.Name, Description: s.Description, Location: s.Location})
	}
	assert.Equal(t, want, withNil)
}

// TestReadSkill_MarketThirdSource verifies the read path: local miss →
// allowlist hit → SKILL.md body (frontmatter stripped) from the market dir.
func TestReadSkill_MarketThirdSource(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })

	body, err := readSkill(marketCtx(), loader, f.client, "market-skill", "", "u1")
	require.NoError(t, err)
	assert.Equal(t, "Market body.", body)
}

// TestReadSkill_MarketSubPath verifies sub-path reads from a market skill:
// confinement identical to local skills and verbatim text content.
func TestReadSkill_MarketSubPath(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })
	ctx := marketCtx()

	body, err := readSkill(ctx, loader, f.client, "market-skill", "examples/demo.py", "u1")
	require.NoError(t, err)
	assert.Equal(t, "print('market skill asset')", body)

	_, err = readSkill(ctx, loader, f.client, "market-skill", "../../escape", "u1")
	require.Error(t, err, "traversal out of a market skill directory must be rejected")
}

// TestReadSkill_MarketDiskLagErrors verifies a listed-but-unsynced skill
// fails with the explicit directory-not-found error, not a silent miss.
func TestReadSkill_MarketDiskLagErrors(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })

	_, err := readSkill(marketCtx(), loader, f.client, "bar", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found", "sync lag must surface as directory-not-found")
}

// TestReadSkill_LocalWinsOverMarket verifies the precedence on the read path:
// a local skill with the same name as a market skill resolves locally.
func TestReadSkill_LocalWinsOverMarket(t *testing.T) {
	f := newMarketFixture(t)
	globalDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "foo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "foo", "SKILL.md"),
		[]byte("---\nname: foo\ndescription: local\n---\nLOCAL foo body\n"), 0o644))
	loader := skill.NewLoader(globalDir, func(userID string) string { return t.TempDir() })

	body, err := readSkill(marketCtx(), loader, f.client, "foo", "", "u1")
	require.NoError(t, err)
	assert.Equal(t, "LOCAL foo body", body)
}

// TestReadSkill_MarketMissStillNotFound verifies a name in neither local nor
// allowlist keeps the plain skill-not-found error.
func TestReadSkill_MarketMissStillNotFound(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })

	_, err := readSkill(marketCtx(), loader, f.client, "nonexistent", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestListSkillFiles_MarketSkill verifies luban_list_skill_files resolves a
// market skill and lists its directory with the local-skill shape.
func TestListSkillFiles_MarketSkill(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })
	ctx := marketCtx()

	res, err := ListSkillFiles(ctx, loader, f.client, "market-skill", "", "u1", false)
	require.NoError(t, err)
	got := res.(listSkillFilesResult)
	assert.Equal(t, ".", got.Path)
	names := make([]string, 0, len(got.Entries))
	for _, e := range got.Entries {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"SKILL.md", "examples"}, names)

	// Disk-lagged market skill: resolution succeeds (allowlist hit), stat fails.
	_, err = ListSkillFiles(ctx, loader, f.client, "bar", "", "u1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found")

	// Name in neither source: skill not found.
	_, err = ListSkillFiles(ctx, loader, f.client, "nope", "", "u1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestTreeSkill_MarketSkill verifies luban_tree_skill resolves a market skill
// and renders its tree with the local-skill shape.
func TestTreeSkill_MarketSkill(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })

	res, err := TreeSkill(marketCtx(), loader, f.client, "market-skill", "", "u1", 0, false)
	require.NoError(t, err)
	got := res.(treeSkillResult)
	assert.Equal(t, ".", got.Path)
	require.Len(t, got.Tree, 2)
	assert.Equal(t, "SKILL.md", got.Tree[0].Name)
	assert.Equal(t, "examples", got.Tree[1].Name)
	assert.NotEmpty(t, got.Tree[1].Children)

	// Sub-path traversal inside a market skill is rejected identically.
	_, err = TreeSkill(marketCtx(), loader, f.client, "market-skill", "../../etc", "u1", 0, false)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "outside"), "traversal error: %v", err)
}

// TestMarketSkillDir_NoTokenFailsClosed verifies the luban fallback degrades
// to a miss (never an error, never a scan) when the token never reached the
// context — the same fail-closed contract as the client.
func TestMarketSkillDir_NoTokenFailsClosed(t *testing.T) {
	f := newMarketFixture(t)
	loader := skill.NewLoader(t.TempDir(), func(userID string) string { return t.TempDir() })

	// No token: allowlist skipped → market miss → skill not found.
	_, err := readSkill(context.Background(), loader, f.client, "market-skill", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
