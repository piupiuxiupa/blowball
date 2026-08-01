// Package mcp implements the per-user MCP tool family: a workspace-resident
// configuration store (`.blowball/mcp/{name}/config.json`, one directory per
// server), a turn-scoped connection manager, and the agent-facing
// `mcp_list_servers` / `mcp_add_server` / `mcp_remove_server` / `mcp_call`
// tools.
//
// Per-user MCP is intentionally narrower than operator MCP: only the remote
// HTTP (Streamable HTTP) transport and only static credentials (bearer /
// api-key / basic) are supported. The three leak invariants are enforced here
// and guarded by tests: (1) management-tool output redacts credentials, (2)
// MCP client logging never echoes auth headers, (3) `mcp_call` injects auth
// server-side so the value never reaches model-visible text.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// configDir is the per-user MCP config directory beneath the workspace, a
// sibling of the per-user skills dir under the reserved `.blowball` namespace.
const configDir = ".blowball/mcp"

// ConfigFile is the config filename inside each server directory.
const ConfigFile = "config.json"

// serversDir returns the per-user MCP servers directory beneath workspaceRoot
// (`.blowball/mcp/`). Each immediate subdirectory whose name passes
// ValidateName is one server; its config lives at serverConfigPath.
func serversDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, configDir)
}

// serverConfigPath returns the absolute path of one server's config file:
// `{workspaceRoot}/.blowball/mcp/{name}/config.json`. name is the server
// identity (and the directory name); it is NOT written into the file body.
// Callers MUST validate name with ValidateName before using it as a path
// component.
func serverConfigPath(workspaceRoot, name string) string {
	return filepath.Join(serversDir(workspaceRoot), name, ConfigFile)
}

// nameRegexp constrains a server name to a single safe path segment: it must
// start with an ASCII letter or digit and contain only letters, digits,
// underscore, and hyphen, with a total length of 1–64. This forbids path
// separators, leading dots, whitespace, and other characters that would allow
// traversal, nesting, or hidden-directory attacks once the name becomes a
// directory component.
var nameRegexp = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// errInvalidName is the base error for a name that fails ValidateName. It is
// formatted once (with the rules and examples) and wrapped with the offending
// name at each call site so every rejection carries the same guidance.
const nameRules = `must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$ (start with a letter or digit; ` +
	`only letters, digits, "_" and "-"; 1-64 chars). Good: "github", "my-mcp", "svc_2". ` +
	`Bad: names with spaces, dots, or slashes, or a leading dot, e.g. "a.b", "a/b", "a b", ".hidden", "../x"`

// ValidateName reports whether name is a safe server identifier / path
// component. It is the single source of truth used by the add path, the load
// path (enumeration skips names that fail it), and the write path, so no
// invalid name can ever reach the filesystem.
func ValidateName(name string) error {
	if !nameRegexp.MatchString(name) {
		return fmt.Errorf("mcp server name %q is invalid: %s", name, nameRules)
	}
	return nil
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
//
// Name is the server identity and the on-disk directory name. It is NOT
// persisted into the file body (`json:"-"`): the directory name is the single
// source of truth, and LoadConfig back-fills it after reading each
// `{name}/config.json`. A stray `name` key inside a file body is therefore
// ignored on load.
type Server struct {
	Name        string      `json:"-"`
	URL         string      `json:"url"`
	Transport   string      `json:"transport"`
	Auth        Auth        `json:"auth,omitempty"`
	Description string      `json:"description,omitempty"`
	Tools       []ToolCache `json:"tools,omitempty"`
}

// Config is the in-memory aggregation of a user's configured servers. It is
// NOT the on-disk shape: each server lives in its own file under
// `.blowball/mcp/{name}/config.json`. LoadConfig reconstructs this struct by
// enumerating those files; the write primitives (WriteServer / AddServer)
// operate per server, never on a single aggregate file.
type Config struct {
	Servers []Server
}

// LoadConfig reads userID's per-user MCP config by enumerating the server
// subdirectories under `{workspaceRoot}/.blowball/mcp/`. A missing directory
// is not an error: it yields an empty Config (no servers).
//
// Enumeration skips any entry that is not a directory, is hidden (leading
// dot), fails ValidateName, or whose `config.json` is missing/unreadable/
// malformed or fails field validation. Skipping a malformed server makes that
// single server unavailable without crashing the process or failing the turn
// (distinct from operator MCP's startup-time fail-fast); the remaining valid
// servers are still returned.
func LoadConfig(workspaceRoot string) (*Config, error) {
	entries, err := os.ReadDir(serversDir(workspaceRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read mcp servers dir %q: %w", serversDir(workspaceRoot), err)
	}
	servers := make([]Server, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue // a stray file is not a server
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // hidden / temp directory
		}
		if err := ValidateName(name); err != nil {
			continue // unsafe path segment — ignore
		}
		s, ok := loadServerFile(workspaceRoot, name)
		if !ok {
			continue // missing or malformed config.json → this server unavailable
		}
		servers = append(servers, s)
	}
	return &Config{Servers: servers}, nil
}

// loadServerFile reads and parses one server's config.json, back-filling the
// directory name as Server.Name. ok=false means the server should be skipped
// (missing file, malformed JSON, or failed field validation) — the caller
// treats it as "this server unavailable" rather than a hard error.
func loadServerFile(workspaceRoot, name string) (Server, bool) {
	path := serverConfigPath(workspaceRoot, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Server{}, false
	}
	var s Server
	if err := json.Unmarshal(data, &s); err != nil {
		return Server{}, false
	}
	if err := validateServer(s); err != nil {
		return Server{}, false
	}
	s.Name = name // directory name is the identity
	return s, true
}

// validateServer checks a single server's persisted fields (url / transport /
// auth). It does NOT check the name (the directory name is authoritative and
// validated separately by ValidateName) or duplicates (directory names are
// unique by construction). It is the shared field validator used by the load,
// write, and add paths.
func validateServer(s Server) error {
	if strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("url is required")
	}
	// Transport defaults to http when omitted (the only supported value);
	// an explicit non-http value is rejected per the transport restriction.
	t := strings.TrimSpace(s.Transport)
	if t == "" {
		t = "http"
	}
	if t != "http" {
		return fmt.Errorf("per-user MCP supports only transport \"http\", got %q (stdio/sse are operator-only)", t)
	}
	if s.Auth.Type == "oauth" || s.Auth.Type == "oauth2" {
		return fmt.Errorf("per-user MCP does not support OAuth flows; use a static bearer/api-key/basic credential")
	}
	// Validate the auth type is one of the supported static kinds (or none).
	switch s.Auth.Type {
	case AuthNone, AuthBearer, AuthAPIKey, AuthBasic, "":
		// ok
	default:
		return fmt.Errorf("unsupported auth type %q (use bearer, api-key, or basic)", s.Auth.Type)
	}
	return nil
}

// Validate enforces the per-user MCP constraints over the in-memory servers:
// each server's name is non-empty and valid, and the per-server field checks
// (url/transport/auth) pass. Duplicate detection is structural (directory
// names are unique) and kept only as a defensive sanity check. It is retained
// as a public sanity hook; the load and write paths validate per server via
// validateServer.
func (c *Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Servers))
	for i, s := range c.Servers {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("servers[%d]: name must be non-empty", i)
		}
		if err := ValidateName(s.Name); err != nil {
			return fmt.Errorf("servers[%d]: %w", i, err)
		}
		if err := validateServer(s); err != nil {
			return fmt.Errorf("servers[%d] (name=%q): %w", i, s.Name, err)
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

// SortedServers returns a shallow copy of the servers sorted by name. Used by
// list rendering so output is deterministic.
func (c *Config) SortedServers() []Server {
	out := make([]Server, len(c.Servers))
	copy(out, c.Servers)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// WriteServer atomically writes a single server's config to
// `{workspaceRoot}/.blowball/mcp/{name}/config.json`, creating the directory
// tree as needed. It validates the name (path-safety) and the server fields
// before writing so a bad entry never lands on disk. The write is atomic (temp
// file + rename in the same directory) so a crash mid-write cannot corrupt
// the server's config. The file body never contains the name.
func WriteServer(workspaceRoot string, server Server) error {
	if err := ValidateName(server.Name); err != nil {
		return err
	}
	if err := validateServer(server); err != nil {
		return fmt.Errorf("mcp server %q: %w", server.Name, err)
	}
	path := serverConfigPath(workspaceRoot, server.Name)
	data, err := json.MarshalIndent(server, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp server %q: %w", server.Name, err)
	}
	return atomicWriteFile(path, data, 0o644)
}

// AddServer creates a new server: it validates the name and fields, refuses to
// overwrite an existing server directory (so a re-add surfaces a clear error
// instead of clobbering), and writes the config via WriteServer. It is the
// canonical "create one server" primitive for the per-server-directory layout.
func AddServer(workspaceRoot string, server Server) error {
	if err := ValidateName(server.Name); err != nil {
		return err
	}
	if err := validateServer(server); err != nil {
		return fmt.Errorf("mcp server %q: %w", server.Name, err)
	}
	// The server directory must not already exist (avoid overwrite). Using a
	// directory stat makes the duplicate check "directory already exists"
	// semantics rather than an in-memory lookup.
	dir := filepath.Join(serversDir(workspaceRoot), server.Name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("mcp server %q already exists", server.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat mcp server dir %q: %w", dir, err)
	}
	return WriteServer(workspaceRoot, server)
}

// RemoveServer deletes one server's directory (including its config.json),
// leaving every other server untouched. It reports ok=false (with a nil error)
// when the server is not present. An invalid name cannot correspond to a real
// server directory and is treated as not-present rather than rejected, so a
// stale caller cannot trigger an error path.
func RemoveServer(workspaceRoot, name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, nil
	}
	dir := filepath.Join(serversDir(workspaceRoot), name)
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat mcp server dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		// A valid-name entry that is not a directory cannot be a server; treat
		// as not-present rather than deleting an unrelated file.
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove mcp server dir %q: %w", dir, err)
	}
	return true, nil
}

// Defaults for the per-user MCP timeouts. Operators can override them via
// config.yaml (see MCPToolsConfig). connect bounds the handshake; totalCall
// bounds the full connect+initialize+tools/call path of one mcp_call.
const (
	defaultConnectTimeout   = 5 * time.Second
	defaultTotalCallTimeout = 10 * time.Second
)
