// Package mcp implements the per-user MCP tool family: a workspace-resident
// configuration store (`.blowball/mcp/config.json`), a turn-scoped connection
// manager, and the agent-facing `mcp_list_servers` / `mcp_add_server` /
// `mcp_remove_server` / `mcp_call` tools.
//
// Per-user MCP is intentionally narrower than operator MCP: only the remote
// HTTP (Streamable HTTP) transport and only static credentials (bearer /
// api-key / basic) are supported. The three leak invariants are enforced here
// and guarded by tests: (1) management-tool output redacts credentials, (2)
// MCP client logging never echoes auth headers, (3) `mcp_call` injects auth
// server-side so the value never reaches model-visible text.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// configDir is the per-user MCP config directory beneath the workspace, a
// sibling of the per-user skills dir under the reserved `.blowball` namespace.
const configDir = ".blowball/mcp"

// ConfigFile is the config filename inside configDir.
const ConfigFile = "config.json"

// ConfigPath returns the absolute path of userID's per-user MCP config file
// beneath workspaceRoot. It is the single source of truth for the on-disk
// location: the loader, writer, and tests all resolve through it.
func ConfigPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, configDir, ConfigFile)
}

// AuthType enumerates the supported static credential kinds.
type AuthType string

const (
	// AuthNone means no credentials are injected.
	AuthNone AuthType = "none"
	// AuthBearer injects `Authorization: Bearer <value>`.
	AuthBearer AuthType = "bearer"
	// AuthAPIKey injects a custom header (default X-API-Key) carrying the key.
	AuthAPIKey AuthType = "api-key"
	// AuthBasic injects HTTP Basic auth (username:password).
	AuthBasic AuthType = "basic"
)

// Auth holds the static credentials for one server. The secret-bearing fields
// (Value, Password) are NEVER rendered into model-visible output — see
// redactedAuth. Only the injection path (Manager → transport headers) reads
// them.
type Auth struct {
	Type     AuthType `json:"type"`
	Value    string   `json:"value,omitempty"`    // bearer token or api-key value
	Username string   `json:"username,omitempty"` // basic auth username
	Password string   `json:"password,omitempty"` // basic auth password
	// Header is the header name for api-key auth; defaults to X-API-Key when
	// empty. Ignored for other auth types.
	Header string `json:"header,omitempty"`
}

// HasSecret reports whether the auth carries a credential that must be kept out
// of model-visible text. AuthNone and an empty Auth both report false.
func (a Auth) HasSecret() bool {
	switch a.Type {
	case AuthBearer, AuthAPIKey, AuthBasic:
		return true
	}
	return false
}

// ToolCache is the cached tools/list snapshot for one server, written by
// mcp_add_server and consulted by mcp_call to validate tool name and args
// before issuing the remote call.
type ToolCache struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Server is one per-user MCP server entry. It mirrors the operator
// MCPServerConfig but is scoped to the HTTP transport and static auth that the
// per-user path supports.
type Server struct {
	Name        string      `json:"name"`
	URL         string      `json:"url"`
	Transport   string      `json:"transport"`
	Auth        Auth        `json:"auth,omitempty"`
	Description string      `json:"description,omitempty"`
	Tools       []ToolCache `json:"tools,omitempty"`
}

// Config is the on-disk shape of `.blowball/mcp/config.json`. The canonical
// write form is an object with a `servers` array; the loader also tolerates a
// bare JSON array for forward compatibility.
type Config struct {
	Servers []Server `json:"servers"`
}

// LoadConfig reads and validates userID's per-user MCP config. A missing file
// is not an error: it yields an empty Config (no servers). A malformed file is
// an error but must NOT crash the process — callers surface it to the agent as
// a per-operation tool error.
func LoadConfig(workspaceRoot string) (*Config, error) {
	path := ConfigPath(workspaceRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read mcp config %q: %w", path, err)
	}

	cfg, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse mcp config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("mcp config %q: %w", path, err)
	}
	return cfg, nil
}

// parseConfig accepts either {"servers":[...]} or a bare [...]. The canonical
// form is the object; the bare-array form is tolerated so a hand-edited file
// keeps working.
func parseConfig(data []byte) (*Config, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return &Config{}, nil
	}
	if trimmed[0] == '[' {
		var servers []Server
		if err := json.Unmarshal(data, &servers); err != nil {
			return nil, err
		}
		return &Config{Servers: servers}, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces the per-user MCP constraints: transport must be http, auth
// must not require an OAuth flow, names must be unique and carry the required
// fields. It is called by LoadConfig and by AddServer/RemoveServer write paths
// before persisting so a bad write can never land on disk.
func (c *Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Servers))
	for i, s := range c.Servers {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("servers[%d]: name must be non-empty", i)
		}
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("servers[%d] (name=%q): url is required", i, s.Name)
		}
		// Transport defaults to http when omitted (the only supported value);
		// an explicit non-http value is rejected per the transport restriction.
		t := strings.TrimSpace(s.Transport)
		if t == "" {
			t = "http"
		}
		if t != "http" {
			return fmt.Errorf("servers[%d] (name=%q): per-user MCP supports only transport \"http\", got %q (stdio/sse are operator-only)", i, s.Name, t)
		}
		if s.Auth.Type == "oauth" || s.Auth.Type == "oauth2" {
			return fmt.Errorf("servers[%d] (name=%q): per-user MCP does not support OAuth flows; use a static bearer/api-key/basic credential", i, s.Name)
		}
		// Validate the auth type is one of the supported static kinds (or none).
		switch s.Auth.Type {
		case AuthNone, AuthBearer, AuthAPIKey, AuthBasic, "":
			// ok
		default:
			return fmt.Errorf("servers[%d] (name=%q): unsupported auth type %q (use bearer, api-key, or basic)", i, s.Name, s.Auth.Type)
		}
		if _, exists := seen[s.Name]; exists {
			return fmt.Errorf("servers: duplicate server name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// Server returns the named server and ok=true, or (Server{}, false).
func (c *Config) Server(name string) (Server, bool) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return Server{}, false
}

// index returns the position of name in c.Servers, or -1.
func (c *Config) index(name string) int {
	for i, s := range c.Servers {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// WriteConfig atomically writes cfg to userID's config file, creating the
// `.blowball/mcp/` directory tree as needed. It validates before writing so a
// bad entry never lands on disk. The write is atomic (temp file + rename) so a
// crash mid-write cannot corrupt the config.
func WriteConfig(workspaceRoot string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("write mcp config: nil config")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	path := ConfigPath(workspaceRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create mcp config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp config: %w", err)
	}
	return atomicWriteFile(path, data, 0o644)
}

// AddServer appends server to cfg after validating uniqueness and the per-user
// constraints, returning an error without mutating cfg on a duplicate name.
// The caller is responsible for calling WriteConfig to persist.
func (cfg *Config) AddServer(server Server) error {
	if strings.TrimSpace(server.Name) == "" {
		return fmt.Errorf("server name is empty")
	}
	if cfg.index(server.Name) >= 0 {
		return fmt.Errorf("mcp server %q already exists", server.Name)
	}
	// Normalize transport default so the persisted entry is canonical.
	if strings.TrimSpace(server.Transport) == "" {
		server.Transport = "http"
	}
	// Full validation (transport/auth/required fields) before mutating.
	next := *cfg
	next.Servers = append(next.Servers, server)
	if err := next.Validate(); err != nil {
		return err
	}
	cfg.Servers = next.Servers
	return nil
}

// RemoveServer removes the named server from cfg, returning ok=false when it is
// not present. It preserves the order of the remaining servers.
func (cfg *Config) RemoveServer(name string) bool {
	i := cfg.index(name)
	if i < 0 {
		return false
	}
	cfg.Servers = append(cfg.Servers[:i], cfg.Servers[i+1:]...)
	return true
}

// SortedServers returns a shallow copy of the servers sorted by name. Used by
// list rendering so output is deterministic.
func (c *Config) SortedServers() []Server {
	out := make([]Server, len(c.Servers))
	copy(out, c.Servers)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Defaults for the per-user MCP timeouts. Operators can override them via
// config.yaml (see MCPToolsConfig). connect bounds the handshake; totalCall
// bounds the full connect+initialize+tools/call path of one mcp_call.
const (
	defaultConnectTimeout   = 5 * time.Second
	defaultTotalCallTimeout = 10 * time.Second
)
