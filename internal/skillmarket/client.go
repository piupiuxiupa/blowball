// Package skillmarket implements the remote-authorized skill market source
// (skill-market capability): it fetches the per-user skill allowlist from an
// external market service using the caller's login JWT, caches it per user
// for a configurable TTL, and resolves allowlist entries to on-disk
// directories under {data-dir}/skills-market (which the operator syncs from
// the market platform).
//
// Authorization and content distribution are deliberately separated: the
// market service decides WHAT a user may see (the allowlist); the operator
// syncs the payloads onto every host. blowball strictly exposes only paths
// the allowlist returns — every failure (down, timeout, non-200, bad JSON,
// auth failure, missing token) degrades FAIL-CLOSED to an empty allowlist and
// there is NEVER a directory-scan fallback (scanning the disk would amount to
// authorization-free exposure of other users' skills).
package skillmarket

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
	"go.uber.org/zap"
)

// maxAllowlistBytes caps the allowlist response body so a hostile or broken
// market service cannot balloon process memory through an unbounded JSON
// payload.
const maxAllowlistBytes = 4 << 20 // 4MB

// LocationSkillMarket is the `location` value labeling skill-market entries in
// luban_list_skills output and the GET /api/v1/skills response (local sources
// stay "global"/"user"; a name conflict resolves local-first, so a local entry
// never carries this label).
const LocationSkillMarket = "skill_market"

// tokenCtxKey is the unexported context key carrying the turn's raw login JWT
// (the agent.WithSessionID precedent: an unexported struct type keyed value
// that only this package reads back).
type tokenCtxKey struct{}

// WithToken returns a copy of ctx carrying the raw login Bearer token so the
// market client can authenticate the per-user allowlist fetch from deep
// inside tool execution without threading the token through the agent APIs.
// MessageStreamHandler injects it once per request; the turn context
// inherits it. An empty token returns ctx unchanged (matching
// agent.WithSessionID).
func WithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenCtxKey{}, token)
}

// TokenFromContext returns the raw login token stored in ctx, or the empty
// string if none is present.
func TokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tokenCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// Entry is one skill in the market allowlist, mirroring the market service's
// response shape {"skills":[{slug,name,description,role,path}]}. The cache
// stores these verbatim — disk presence is checked at the USE point (list
// shows API values even when sync lags; read/mount error or skip), never
// folded into the cached state.
type Entry struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`        // the skill-name resolution key (luban primary key)
	Description string `json:"description"` // list display value
	Role        string `json:"role"`        // ignored: not filtered, not branched on
	Path        string `json:"path"`        // "/{market_user_id}/{skill_name}" relative to the market root
}

// NamedDir is one allowlist entry resolved to its on-disk directory, as
// consumed by the bash sandbox's per-skill --ro-bind mounts.
type NamedDir struct {
	Name string
	Dir  string
}

// Client is the process-wide skill-market facade. It holds ONE shared
// *http.Client (connection pooling + concurrency safety) with Timeout
// deliberately 0 — the deadline is enforced per call via the request context
// (the internal/memory.Service shape). A nil *Client means the capability is
// disabled; Enabled is nil-safe so callers keep a single call shape.
type Client struct {
	cfg       config.SkillMarketConfig
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
// callers hold the zero-wiring shape unchanged (the memory.NewService
// precedent).
func New(cfg config.SkillMarketConfig, marketDir string) *Client {
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

// Allowlist returns userID's skill allowlist, served from the per-user TTL
// cache when fresh and fetched over HTTP otherwise. The raw login JWT rides
// the Authorization: Bearer header from ctx (TokenFromContext); the market
// service resolves the user identity from it. Every failure — missing token,
// unreachable host, timeout, non-200, malformed JSON — logs one WARN and
// returns nil (fail-closed; failures are NOT cached, so the next call retries
// within its own budget). Callers must treat a nil/empty result as "no market
// skills", never as an error.
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
	token := TokenFromContext(ctx)
	if token == "" {
		logger.L().Warn("skill market allowlist skipped: no login token in context (fail-closed)",
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
	Skills []Entry `json:"skills"`
}

// fetch performs the GET with the caller's JWT and parses the allowlist. Any
// error path returns nil after one WARN — the fail-closed contract. The
// response body is capped at maxAllowlistBytes.
func (c *Client) fetch(ctx context.Context, token, userID string) []Entry {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimSpace(c.cfg.URL), nil)
	if err != nil {
		logger.L().Warn("skill market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		logger.L().Warn("skill market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.L().Warn("skill market allowlist fetch failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Int("http_status", resp.StatusCode))
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAllowlistBytes))
	if err != nil {
		logger.L().Warn("skill market allowlist read failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	var parsed allowlistResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		logger.L().Warn("skill market allowlist parse failed (fail-closed)",
			zap.String("user_id", userID),
			zap.Error(err))
		return nil
	}
	return parsed.Skills
}

// ResolveDir returns the on-disk directory for the FIRST allowlist entry
// named name (the luban name-resolution key; duplicates resolve to the first,
// mirroring the sandbox bind rule). The entry's path is joined under the
// market root and guarded: an empty name, an invalid name, or a path that
// cleans outside the root yields ("", false) — escape attempts log one WARN
// and the entry is dropped.
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
// allowlist order, with duplicate names collapsing to the first (the sandbox
// bind rule) and entries whose path escapes the market root — or whose name
// is not a simple identifier — dropped with a WARN. It feeds the bash
// sandbox's per-skill --ro-bind generation.
func (c *Client) Dirs(allow []Entry) []NamedDir {
	if c == nil {
		return nil
	}
	out := make([]NamedDir, 0, len(allow))
	seen := make(map[string]struct{}, len(allow))
	for _, e := range allow {
		if !validName(e.Name) {
			logger.L().Warn("skill market entry dropped: name is not a simple identifier",
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
		out = append(out, NamedDir{Name: e.Name, Dir: dir})
	}
	return out
}

// entryDir joins an entry's API path under the market root and asserts the
// cleaned result stays inside it. The API path is semi-trusted input: a
// hostile or buggy market response must never steer reads or binds outside
// {data-dir}/skills-market.
func (c *Client) entryDir(e Entry) (string, bool) {
	p := strings.TrimSpace(e.Path)
	if p == "" {
		logger.L().Warn("skill market entry dropped: empty path",
			zap.String("name", e.Name))
		return "", false
	}
	joined := filepath.Clean(filepath.Join(c.marketDir, p))
	if !pathWithin(joined, c.marketDir) {
		logger.L().Warn("skill market entry dropped: path escapes the market root",
			zap.String("name", e.Name),
			zap.String("path", e.Path),
			zap.String("resolved", joined))
		return "", false
	}
	return joined, true
}

// validName reports whether name is a simple identifier usable as a mount
// target segment and a map key: non-empty, no path separators, no "..".
func validName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Clean(name)
}

// pathWithin reports whether target is root or a path beneath root, using a
// separator-aware prefix check (the skill package's semantics).
func pathWithin(target, root string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// MarketRoot returns the configured on-disk market root ({data-dir}/
// skills-market), Clean'd. Exposed for wiring diagnostics.
func (c *Client) MarketRoot() string {
	if c == nil {
		return ""
	}
	return c.marketDir
}

// String renders the client for log fields without leaking the token.
func (c *Client) String() string {
	if c == nil {
		return "skillmarket.Client(disabled)"
	}
	return fmt.Sprintf("skillmarket.Client(%s)", c.cfg.URL)
}
