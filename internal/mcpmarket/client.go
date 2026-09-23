// Package mcpmarket implements the remote-authorized MCP market source
// (mcp-market capability): it fetches the per-user MCP server allowlist from
// an external market service using the caller's login JWT, caches it per user
// for a configurable TTL, and resolves allowlist entries to on-disk
// directories under {data-dir}/mcp-market (which the operator syncs from the
// market platform).
//
// Authorization and content distribution are deliberately separated (the
// skillmarket shape): the market service decides WHAT a user may see; the
// operator syncs the payloads onto every host. blowball strictly exposes only
// paths the allowlist returns — every failure degrades FAIL-CLOSED to an
// empty allowlist and there is NEVER a directory-scan fallback. The market
// tree is read-only to blowball: no writes, no removals.
package mcpmarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/skillmarket"
	"go.uber.org/zap"
)

// maxAllowlistBytes caps the allowlist response body so a hostile or broken
// market service cannot balloon process memory through an unbounded JSON
// payload.
const maxAllowlistBytes = 4 << 20 // 4MB

// Entry is one MCP server in the market allowlist, mirroring the market
// service's response shape {"servers":[{slug,name,description,path}]}. The
// cache stores these verbatim — disk presence is checked at the USE point,
// never folded into the cached state.
type Entry struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`        // the server-name resolution key and shadowing key
	Description string `json:"description"` // list display value
	Path        string `json:"path"`        // "/{market_user_id}/{server_name}" relative to the market root
}

// NamedDir is one allowlist entry resolved to its on-disk directory. The
// Description rides along so consumers can list API values even when disk
// sync lags.
type NamedDir struct {
	Name        string
	Description string
	Dir         string
}

// Client is the process-wide MCP-market facade (the skillmarket shape). A nil
// *Client means the capability is disabled; Enabled is nil-safe so callers
// keep a single call shape.
type Client struct {
	cfg       config.MCPMarketConfig
	marketDir string
	http      *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
	// now is injected time so tests advance the TTL window without sleeping.
	now func() time.Time
}

type cacheEntry struct {
	entries   []Entry
	fetchedAt time.Time
}

// New builds the client. It returns nil for a disabled config (empty url) so
// callers hold the zero-wiring shape unchanged.
func New(cfg config.MCPMarketConfig, marketDir string) *Client {
	if !cfg.IsEnabled() {
		return nil
	}
	return &Client{
		cfg:       cfg,
		marketDir: filepath.Clean(marketDir),
		http:      &http.Client{},
		cache:     make(map[string]cacheEntry),
		now:       time.Now,
	}
}

// Enabled reports whether the capability is wired in. Nil-safe.
func (c *Client) Enabled() bool { return c != nil }

// Allowlist returns userID's MCP server allowlist, served from the per-user
// TTL cache when fresh and fetched over HTTP otherwise. The raw login JWT
// (skillmarket.WithToken — the same turn-context token pipeline) rides the
// Authorization header. Every failure returns nil (fail-closed; failures are
// NOT cached). Callers must treat a nil/empty result as "no market servers",
// never as an error.
func (c *Client) Allowlist(ctx context.Context, userID string) []Entry {
	if c == nil || userID == "" {
		return nil
	}
	if now := c.now(); c.fresh(userID, now) {
		c.mu.Lock()
		cached := c.cache[userID]
		c.mu.Unlock()
		return cloneEntries(cached.entries)
	}
	token := skillmarket.TokenFromContext(ctx)
	if token == "" {
		logger.L().Warn("mcp market allowlist skipped: no login token in context (fail-closed)",
			zap.String("user_id", userID))
		return nil
	}
	entries := c.fetch(ctx, token, userID)
	if entries == nil {
		return nil // fail-closed; do not cache the failure
	}
	c.mu.Lock()
	c.cache[userID] = cacheEntry{entries: cloneEntries(entries), fetchedAt: c.now()}
	c.mu.Unlock()
	return entries
}

// fresh reports whether userID's cache entry is within the TTL window.
// Caller must NOT hold c.mu.
func (c *Client) fresh(userID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.cache[userID]
	return ok && now.Sub(cached.fetchedAt) < c.cfg.CacheTTL
}

// cloneEntries copies the slice so callers can never mutate cached state.
func cloneEntries(in []Entry) []Entry {
	if len(in) == 0 {
		return nil
	}
	out := make([]Entry, len(in))
	copy(out, in)
	return out
}

// allowlistResponse is the wire shape of the market service's response.
type allowlistResponse struct {
	Servers []Entry `json:"servers"`
}

// fetch performs the GET with the caller's JWT and parses the allowlist. Any
// error path returns nil after one WARN — the fail-closed contract.
func (c *Client) fetch(ctx context.Context, token, userID string) []Entry {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimSpace(c.cfg.URL), nil)
	if err != nil {
		logger.L().Warn("mcp market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		logger.L().Warn("mcp market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.L().Warn("mcp market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Int("http_status", resp.StatusCode))
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAllowlistBytes))
	if err != nil {
		logger.L().Warn("mcp market allowlist read failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	var parsed allowlistResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		logger.L().Warn("mcp market allowlist parse failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	return parsed.Servers
}

// ResolveDir returns the on-disk directory for the FIRST allowlist entry
// named name. An empty name, an invalid name, or a path that cleans outside
// the root yields ("", false) — escape attempts log one WARN and the entry
// is dropped.
func (c *Client) ResolveDir(allow []Entry, name string) (string, bool) {
	if c == nil || !validName(name) {
		return "", false
	}
	for _, e := range allow {
		if e.Name != name {
			continue
		}
		dir, ok := c.entryDir(e)
		if !ok {
			return "", false
		}
		return dir, true
	}
	return "", false
}

// Dirs resolves every allowlist entry to its on-disk directory, preserving
// allowlist order, with duplicate names collapsing to the first and entries
// whose path escapes the market root — or whose name is not a simple
// identifier — dropped with a WARN.
func (c *Client) Dirs(allow []Entry) []NamedDir {
	if c == nil {
		return nil
	}
	out := make([]NamedDir, 0, len(allow))
	seen := make(map[string]struct{}, len(allow))
	for _, e := range allow {
		if !validName(e.Name) {
			logger.L().Warn("mcp market entry dropped: name is not a simple identifier",
				zap.String("name", e.Name))
			continue
		}
		if _, dup := seen[e.Name]; dup {
			continue
		}
		dir, ok := c.entryDir(e)
		if !ok {
			continue
		}
		seen[e.Name] = struct{}{}
		out = append(out, NamedDir{Name: e.Name, Description: e.Description, Dir: dir})
	}
	return out
}

// entryDir resolves one entry's path under the market root with the
// Join+Clean+within-root guard (path is semi-trusted input). A path that
// escapes the root is dropped with a WARN.
func (c *Client) entryDir(e Entry) (string, bool) {
	if e.Path == "" {
		logger.L().Warn("mcp market entry dropped: empty path",
			zap.String("name", e.Name))
		return "", false
	}
	joined := filepath.Clean(filepath.Join(c.marketDir, e.Path))
	if !pathWithin(joined, c.marketDir) {
		logger.L().Warn("mcp market entry dropped: path escapes the market root",
			zap.String("name", e.Name),
			zap.String("path", e.Path),
			zap.String("resolved", joined))
		return "", false
	}
	return joined, true
}

// validName reports whether name is a simple identifier usable as a map key
// and shadowing key: non-empty, no path separators, no "..".
func validName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Clean(name)
}

// pathWithin reports whether target is root or a path beneath root, using a
// separator-aware prefix check (the skillmarket semantics).
func pathWithin(target, root string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// MarketRoot returns the configured on-disk market root
// ({data-dir}/mcp-market), Clean'd. Exposed for wiring diagnostics.
func (c *Client) MarketRoot() string {
	if c == nil {
		return ""
	}
	return c.marketDir
}

// String renders the client for log fields without leaking the token.
func (c *Client) String() string {
	if c == nil {
		return "mcpmarket.Client(disabled)"
	}
	return fmt.Sprintf("mcpmarket.Client(%s)", c.cfg.URL)
}
