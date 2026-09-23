package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lush/blowball/internal/mcpmarket"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/tool/skill"
	"go.uber.org/zap"
)

// This file implements the MCP market as the per-user tools' SECOND
// name-resolution source (mcp-market capability): workspace > market. The
// local LoadConfig stays untouched; a workspace server of the same name
// shadows the market entry (the skill-market local-first precedent). A nil
// market client means the capability is off and every function here
// degrades to workspace-only behavior.

// LoadServersWithMarket returns the caller's servers with market entries
// merged in (workspace-first shadowing). It is the shared merge point for
// mcp_list_servers, the system-prompt advertisement, and the MCP tools
// endpoint. A nil market returns the workspace-only config unchanged.
func LoadServersWithMarket(ctx context.Context, userID, workspaceRoot string, market *mcpmarket.Client) (*Config, error) {
	cfg, err := LoadConfig(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if market == nil || userID == "" {
		return cfg, nil
	}
	local := make(map[string]struct{}, len(cfg.Servers))
	for _, s := range cfg.Servers {
		local[s.Name] = struct{}{}
	}
	for _, s := range marketServers(ctx, userID, market) {
		if _, shadowed := local[s.Name]; shadowed {
			continue
		}
		cfg.Servers = append(cfg.Servers, s)
	}
	return cfg, nil
}

// lookupServer resolves name workspace-first, then through the market
// fallback. It is the ONE resolution path shared by mcp_call / mcp_list_tools
// / Manager.Conn so the precedence rules live in a single place.
func lookupServer(ctx context.Context, m *Manager, name string) (Server, error) {
	cfg, err := LoadConfig(m.WorkspaceRoot())
	if err != nil {
		return Server{}, err
	}
	if s, ok := cfg.Server(name); ok {
		return s, nil
	}
	return marketServer(ctx, skill.UserIDFromContext(ctx), m.market, name)
}

// marketServer resolves one named market server to its config.json. A name
// outside the allowlist (or a nil market) yields the same "not configured"
// error as a workspace miss; an allowlisted name whose payload has not been
// synced to disk yet yields the disk-sync-lag error so the entry is visibly
// broken rather than silently hidden.
func marketServer(ctx context.Context, userID string, market *mcpmarket.Client, name string) (Server, error) {
	if market == nil {
		return Server{}, fmt.Errorf("mcp server %q is not configured", name)
	}
	dir, ok := market.ResolveDir(market.Allowlist(ctx, userID), name)
	if !ok {
		return Server{}, fmt.Errorf("mcp server %q is not configured", name)
	}
	return readMarketServerFile(dir, name)
}

// marketServers resolves the whole allowlist into Server values, preserving
// allowlist order. Listing follows the API-truth contract: an entry whose
// config.json is not on disk yet still appears (name/description from the
// allowlist); an entry whose config.json is malformed or fails validation is
// skipped with a WARN.
func marketServers(ctx context.Context, userID string, market *mcpmarket.Client) []Server {
	out := make([]Server, 0, 8)
	for _, nd := range market.Dirs(market.Allowlist(ctx, userID)) {
		s, err := readMarketServerFile(nd.Dir, nd.Name)
		if err != nil {
			if errors.Is(err, errMarketDiskLag) {
				out = append(out, Server{Name: nd.Name, Description: nd.Description, market: true})
				continue
			}
			continue // malformed/unvalidated entry: already logged, skipped
		}
		out = append(out, s)
	}
	return out
}

// errMarketDiskLag marks an allowlisted entry whose payload directory has not
// reached this host yet (the statMarketDir precedent from luban).
var errMarketDiskLag = errors.New("market disk sync incomplete")

// readMarketServerFile reads and validates one market server's config.json,
// back-filling the allowlist name as Server.Name. Unlike the workspace
// loadServerFile, the name comes from the allowlist (the resolution key), not
// the directory name.
func readMarketServerFile(dir, name string) (Server, error) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Server{}, fmt.Errorf("%w: market server %q: config.json not found", errMarketDiskLag, name)
		}
		return Server{}, fmt.Errorf("read market server %q config: %w", name, err)
	}
	var s Server
	if err := json.Unmarshal(data, &s); err != nil {
		logger.L().Warn("mcp market server skipped: malformed config.json",
			zap.String("server", name),
			zap.Error(err))
		return Server{}, err
	}
	if err := ValidateName(name); err != nil {
		logger.L().Warn("mcp market server skipped: invalid name",
			zap.String("server", name))
		return Server{}, fmt.Errorf("invalid market server name %q", name)
	}
	if err := validateServer(s); err != nil {
		logger.L().Warn("mcp market server skipped: invalid config",
			zap.String("server", name),
			zap.Error(err))
		return Server{}, err
	}
	s.Name = name
	s.market = true
	return s, nil
}
