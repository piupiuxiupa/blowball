package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool"
)

// newMarketTools builds an executor Tools bundle wired to an httptest market
// service over a temp market root carrying one synced skill (synced-skill) and
// one listed-but-unsynced skill (lagging-skill).
func newMarketTools(t *testing.T) (*Tools, *httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	synced := filepath.Join(root, "uid-1", "synced-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(synced, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(synced, "SKILL.md"), []byte("m"), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-exec" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"skills":[
			{"name":"synced-skill","description":"d1","path":"/uid-1/synced-skill"},
			{"name":"lagging-skill","description":"d2","path":"/uid-1/lagging-skill"},
			{"name":"other-user-skill","description":"d3","path":"/uid-2/other-user-skill"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := config.SkillMarketConfig{URL: srv.URL, Timeout: 2 * time.Second, CacheTTL: time.Minute}
	client := skillmarket.New(cfg, root)
	require.NotNil(t, client)
	tools := NewTools(config.ExecutorConfig{}, func(userID string) string { return "/ws/" + userID }, "/skills-dir", "/tools-dir").WithMarket(client)
	return tools, srv, root
}

func execCtx(userID string) context.Context {
	return skillmarket.WithToken(context.Background(), "jwt-exec")
}

// TestMarketBinds_AuthorizedSyncedSkillMounted verifies the authorized+synced
// skill yields exactly one flattened bind; unsynced (stat-guard skipped) and
// other-user skills never appear.
func TestMarketBinds_AuthorizedSyncedSkillMounted(t *testing.T) {
	tools, _, _ := newMarketTools(t)
	binds := tools.marketBinds(execCtx("u1"), "u1")
	require.Len(t, binds, 1, "only the synced, allowlisted skill binds")
	assert.Equal(t, filepath.Join(tools.market.MarketRoot(), "uid-1", "synced-skill"), binds[0].Host)
	assert.Equal(t, "/skills/market/synced-skill", binds[0].Target)
}

// TestMarketBinds_UnauthorizedZeroMounts verifies a user the market does not
// authorize (401 for a foreign token / empty allowlist) gets zero mounts.
func TestMarketBinds_UnauthorizedZeroMounts(t *testing.T) {
	tools, _, _ := newMarketTools(t)
	// No token: fail-closed, zero binds.
	assert.Empty(t, tools.marketBinds(context.Background(), "u1"))
	// Empty userID: no binds.
	assert.Empty(t, tools.marketBinds(execCtx("u1"), ""))
}

// TestMarketBinds_MarketFailureZeroMounts verifies a dead market fail-closes
// to zero binds (bash stays usable).
func TestMarketBinds_MarketFailureZeroMounts(t *testing.T) {
	tools, srv, _ := newMarketTools(t)
	srv.Close()
	assert.Empty(t, tools.marketBinds(execCtx("u1"), "u1"))
}

// TestMarketBinds_NilDependencyZeroMounts verifies a nil client (capability
// off) yields no binds at all.
func TestMarketBinds_NilDependencyZeroMounts(t *testing.T) {
	tools := NewTools(config.ExecutorConfig{}, nil, "/skills-dir", "/tools-dir")
	assert.Nil(t, tools.marketBinds(execCtx("u1"), "u1"))
}

// TestBuildBwrapArgs_MarketBindsAppended verifies the market binds land in the
// bwrap args as --ro-bind triples after the fixed invariants (before the
// system baseline), and that omitting them reproduces the pre-capability arg
// list exactly.
func TestBuildBwrapArgs_MarketBindsAppended(t *testing.T) {
	sandbox := defaultSandbox()
	cfg := config.ExecutorToolConfig{}
	base := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/data/tools", sandbox, cfg)
	withMarket := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/data/tools", sandbox, cfg,
		MarketBind{Host: "/market/uid-1/skill-a", Target: "/skills/market/skill-a"},
		MarketBind{Host: "/market/uid-1/skill-b", Target: "/skills/market/skill-b"},
	)

	// No binds passed (variadic omitted) == legacy call shape == the same arg
	// multiset (the trailing --setenv block iterates a map, so two builds may
	// legitimately differ in env ordering; positions are pinned below).
	noBinds := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/data/tools", sandbox, cfg)
	sortedCopy := func(in []string) []string {
		out := append([]string{}, in...)
		slices.Sort(out)
		return out
	}
	assert.Equal(t, sortedCopy(base), sortedCopy(noBinds))
	assert.Len(t, noBinds, len(base))

	// The market triples sit directly after the fixed invariants: strip them
	// from withMarket and the remainder is exactly the legacy arg list.
	marketTriples := []string{
		"--ro-bind", "/market/uid-1/skill-a", "/skills/market/skill-a",
		"--ro-bind", "/market/uid-1/skill-b", "/skills/market/skill-b",
	}
	chdirAt := func(args []string) int {
		for i, a := range args {
			if a == "--chdir" {
				return i + 2 // --chdir /workspace are the last fixed invariants
			}
		}
		t.Fatal("fixed invariants missing --chdir")
		return -1
	}
	cut := chdirAt(withMarket)
	require.Less(t, cut+6, len(withMarket))
	assert.Equal(t, marketTriples, withMarket[cut:cut+6])
	// Removing the market triples reproduces the legacy args (order-insensitive
	// for the map-ordered --setenv tail; positions pinned above).
	stripped := append(append([]string{}, withMarket[:cut]...), withMarket[cut+6:]...)
	assert.Equal(t, sortedCopy(base), sortedCopy(stripped), "removing the market triples must reproduce the legacy args")
	assert.Len(t, stripped, len(base))
}

// TestBashDescription_MarketPathConvention verifies the bash tool description
// documents the /skills/market/{skill-name}/ path convention.
func TestBashDescription_MarketPathConvention(t *testing.T) {
	reg := tool.NewRegistry()
	tools := NewTools(config.ExecutorConfig{}, nil, "/skills-dir", "/tools-dir")
	require.NoError(t, registerBash(reg, tools))
	spec, ok := reg.Get(ToolBash)
	require.True(t, ok)
	assert.Contains(t, spec.Description, "/skills/market/{skill-name}/")
	assert.Contains(t, spec.Description, "skill_market")
	// Sanity: the description still renders as part of the OpenAI tools shape.
	raw, err := reg.OpenAITools([]string{ToolBash})
	require.NoError(t, err)
	var rendered []struct {
		Function struct {
			Description string `json:"description"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal(raw, &rendered))
	require.Len(t, rendered, 1)
	assert.True(t, strings.Contains(rendered[0].Function.Description, "/skills/market/{skill-name}/"))
}
