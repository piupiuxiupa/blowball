package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/mcpmarket"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/skill"
)

// marketFixture wires an mcpmarket.Client against a fake allowlist server and
// a temp on-disk market root: {root}/{mid}/{name}/config.json.
type marketFixture struct {
	client *mcpmarket.Client
	root   string
}

func newMarketFixture(t *testing.T, entries ...mcpmarket.Entry) *marketFixture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := struct {
			Servers []mcpmarket.Entry `json:"servers"`
		}{Servers: entries}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	return &marketFixture{
		client: mcpmarket.New(config.MCPMarketConfig{URL: srv.URL, Timeout: 2 * time.Second, CacheTTL: time.Minute}, root),
		root:   root,
	}
}

// writeMarketConfig lands one operator-synced config.json under the market root.
func (f *marketFixture) writeMarketConfig(t *testing.T, path, name, body string) {
	t.Helper()
	dir := filepath.Join(f.root, filepath.FromSlash(path))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o644))
}

func (f *marketFixture) readMarketConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path), ConfigFile))
	require.NoError(t, err)
	return string(data)
}

// userCtx carries both the market token and the userID the tool layer reads.
func userCtx(userID string) context.Context {
	return skill.WithUserID(skillmarket.WithToken(context.Background(), "jwt-test"), userID)
}

func TestLoadServersWithMarket_MergeShadowAndDiskLag(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, WriteServer(ws, Server{Name: "local", URL: "http://local/mcp", Transport: "http"}))
	require.NoError(t, WriteServer(ws, Server{Name: "shared", URL: "http://ws-shared/mcp", Transport: "http"}))

	f := newMarketFixture(t,
		mcpmarket.Entry{Name: "marketed", Description: "Market only", Path: "mid1/marketed"},
		mcpmarket.Entry{Name: "shared", Description: "Shadowed by workspace", Path: "mid1/shared"},
		mcpmarket.Entry{Name: "lagging", Description: "Not synced yet", Path: "mid1/lagging"},
	)
	f.writeMarketConfig(t, "mid1/marketed", "marketed", `{"url":"http://market/mcp","transport":"http"}`)
	f.writeMarketConfig(t, "mid1/shared", "shared", `{"url":"http://market-shared/mcp","transport":"http"}`)

	cfg, err := LoadServersWithMarket(userCtx("u1"), "u1", ws, f.client)
	require.NoError(t, err)

	byName := map[string]Server{}
	for _, s := range cfg.Servers {
		byName[s.Name] = s
	}
	require.Len(t, byName, 4, "local + shared(ws) + marketed + lagging")
	assert.Equal(t, "http://local/mcp", byName["local"].URL)
	assert.False(t, byName["local"].market)
	assert.Equal(t, "http://ws-shared/mcp", byName["shared"].URL, "workspace server must shadow the market entry")
	assert.False(t, byName["shared"].market)
	assert.Equal(t, "http://market/mcp", byName["marketed"].URL)
	assert.True(t, byName["marketed"].market)
	// Disk-lag entry still lists (API is the visibility truth), description-only.
	assert.True(t, byName["lagging"].market)
	assert.Equal(t, "Not synced yet", byName["lagging"].Description)
	assert.Empty(t, byName["lagging"].URL)

	// A nil market client is the pre-capability workspace-only shape.
	plain, err := LoadServersWithMarket(userCtx("u1"), "u1", ws, nil)
	require.NoError(t, err)
	require.Len(t, plain.Servers, 2)
}

func TestLookupServer_MarketFallbackAndDiskLag(t *testing.T) {
	ws := t.TempDir()
	f := newMarketFixture(t,
		mcpmarket.Entry{Name: "marketed", Path: "mid1/marketed"},
		mcpmarket.Entry{Name: "lagging", Path: "mid1/lagging"},
	)
	f.writeMarketConfig(t, "mid1/marketed", "marketed", `{"url":"http://market/mcp","transport":"http"}`)

	m := NewManager(ManagerOptions{WorkspaceRoot: ws, Market: f.client})
	defer m.Close()

	ctx := userCtx("u1")
	s, err := lookupServer(ctx, m, "marketed")
	require.NoError(t, err)
	assert.True(t, s.market)
	assert.Equal(t, "http://market/mcp", s.URL)

	_, err = lookupServer(ctx, m, "lagging")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk sync")

	_, err = lookupServer(ctx, m, "unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRemoveServer_MarketReadOnlyAndShadowRestore(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, WriteServer(ws, Server{Name: "shared", URL: "http://ws-shared/mcp", Transport: "http"}))

	f := newMarketFixture(t,
		mcpmarket.Entry{Name: "marketed", Path: "mid1/marketed"},
		mcpmarket.Entry{Name: "shared", Path: "mid1/shared"},
	)
	f.writeMarketConfig(t, "mid1/marketed", "marketed", `{"url":"http://market/mcp","transport":"http"}`)
	f.writeMarketConfig(t, "mid1/shared", "shared", `{"url":"http://market-shared/mcp","transport":"http"}`)

	m := NewManager(ManagerOptions{WorkspaceRoot: ws, Market: f.client})
	defer m.Close()
	ctx := userCtx("u1")

	// Market-only: explicit read-only refusal (not "not configured").
	_, err := removeServer(ctx, m, "marketed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be removed")
	_, statErr := os.Stat(filepath.Join(f.root, "mid1", "marketed", ConfigFile))
	require.NoError(t, statErr, "market tree must be untouched")

	// Workspace server shadowing a market entry removes normally, and the
	// market entry becomes visible again afterwards.
	_, err = removeServer(ctx, m, "shared")
	require.NoError(t, err)
	cfg, err := LoadServersWithMarket(ctx, "u1", ws, f.client)
	require.NoError(t, err)
	for _, s := range cfg.Servers {
		if s.Name == "shared" {
			assert.True(t, s.market, "market entry must resurface after the workspace shadow is removed")
			assert.Equal(t, "http://market-shared/mcp", s.URL)
		}
	}
}

func TestListTools_MarketNoWriteBack(t *testing.T) {
	ws := t.TempDir()
	f := newMarketFixture(t, mcpmarket.Entry{Name: "marketed", Path: "mid1/marketed"})
	const rawCfg = `{"url":"http://market/mcp","transport":"http","tools":[{"name":"stale"}]}`
	f.writeMarketConfig(t, "mid1/marketed", "marketed", rawCfg)

	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "fresh", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	m := NewManager(ManagerOptions{
		WorkspaceRoot:    ws,
		Market:           f.client,
		TransportFactory: func(server Server, _ TimeoutConfig) (mcpclient.Transport, error) { return ft, nil },
	})
	defer m.Close()

	ctx := userCtx("u1")
	views, err := listTools(ctx, m, "marketed")
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, "fresh", views[0].Name)

	// The async write-back must never fire for a market server: the synced
	// config.json stays byte-identical.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, rawCfg, f.readMarketConfig(t, "mid1/marketed"))
}

func TestCallTool_MarketServerRefreshesMemoryOnly(t *testing.T) {
	ws := t.TempDir()
	f := newMarketFixture(t, mcpmarket.Entry{Name: "marketed", Path: "mid1/marketed"})
	const rawCfg = `{"url":"http://market/mcp","transport":"http","tools":[{"name":"stale"}]}`
	f.writeMarketConfig(t, "mid1/marketed", "marketed", rawCfg)

	ft := &fakeTransport{tools: []mcpclient.Tool{{Name: "fresh", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	m := NewManager(ManagerOptions{
		WorkspaceRoot:    ws,
		Market:           f.client,
		TransportFactory: func(server Server, _ TimeoutConfig) (mcpclient.Transport, error) { return ft, nil },
	})
	defer m.Close()

	// "fresh" is absent from the persisted cache, so callTool does one live
	// tools/list refresh — in memory only for a market server.
	res, err := callTool(userCtx("u1"), m, "marketed", "fresh", json.RawMessage(`{}`), "")
	require.NoError(t, err)
	assert.NotNil(t, res)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, rawCfg, f.readMarketConfig(t, "mid1/marketed"), "market config.json must stay read-only")
}

func TestListServers_MarketMergedWithSource(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, WriteServer(ws, Server{Name: "local", URL: "http://local/mcp", Transport: "http"}))

	f := newMarketFixture(t, mcpmarket.Entry{Name: "marketed", Description: "Market only", Path: "mid1/marketed"})
	f.writeMarketConfig(t, "mid1/marketed", "marketed", `{"url":"http://market/mcp","transport":"http","auth":{"type":"bearer","value":"secret"}}`)

	m := NewManager(ManagerOptions{WorkspaceRoot: ws, Market: f.client})
	defer m.Close()

	views, err := listServers(userCtx("u1"), m)
	require.NoError(t, err)
	byName := map[string]serverView{}
	for _, v := range views {
		byName[v.Name] = v
	}
	require.Len(t, byName, 2)
	assert.Empty(t, byName["local"].Source, "workspace servers keep the pre-market wire shape")
	assert.Equal(t, "market", byName["marketed"].Source)
	assert.Equal(t, redacted, byName["marketed"].Auth.Value, "market credentials stay redacted")
}
