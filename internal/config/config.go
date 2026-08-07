// Package config loads and validates the blowball backend configuration.
//
// Configuration is read from a YAML file. Values may reference environment
// variables using the ${VAR} or ${VAR:default} syntax; the loader expands
// them via os.ExpandEnv before unmarshalling.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration tree mirroring config.yaml.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	OpenAI     OpenAIConfig     `yaml:"openai"`
	MySQL      MySQLConfig      `yaml:"mysql"`
	Redis      RedisConfig      `yaml:"redis"`
	Auth       AuthConfig       `yaml:"auth"`
	JWT        JWTConfig        `yaml:"jwt"`
	Agents     AgentsConfig     `yaml:"agents"`
	Tools      ToolsConfig      `yaml:"tools"`
	MCP        MCPConfig        `yaml:"mcp"`
	Landlock   LandlockConfig   `yaml:"landlock"`
	Logging    LoggingConfig    `yaml:"logging"`
	OnlyOffice OnlyOfficeConfig `yaml:"onlyoffice"`
	Storage    StorageConfig    `yaml:"storage"`
}

// WorkspaceBackendLocal is the default workspace storage backend: per-user
// data lives on each process's local disk under {data-dir}/data. This is the
// zero-behavior-change mode and the only supported mode on macOS/Windows dev.
const WorkspaceBackendLocal = "local"

// WorkspaceBackendShared declares that {data-dir}/data is a shared POSIX
// filesystem (operator-mounted JuiceFS backed by MinIO) so per-user data is
// shared across api/agent instances and DR-backed. Shared mode triggers a
// startup mount health check (see the workspace-shared-storage spec).
const WorkspaceBackendShared = "shared"

// defaultWorkspaceBackend is the backend used when storage.workspace.backend is
// omitted, preserving the pre-shared-storage local-disk behavior.
const defaultWorkspaceBackend = WorkspaceBackendLocal

// StorageConfig groups the physical-storage settings for blowball's on-disk
// data (the per-user data root {data-dir}/data: sessions warm tier, workspace,
// per-user skills). It does not affect MySQL or Redis, which are always remote.
type StorageConfig struct {
	Workspace WorkspaceStorageConfig `yaml:"workspace"`
}

// WorkspaceStorageConfig selects where the per-user workspace (and the wider
// per-user data subtree) physically lives. Backend is "local" (default, local
// disk) or "shared" (operator-mounted shared POSIX filesystem such as
// MinIO-backed JuiceFS). The value supports ${VAR} expansion like every other
// config field. In shared mode all existing POSIX file operations stay
// transparent — only the mount point changes — but the server runs a startup
// health check to refuse to boot if the shared mount is missing.
type WorkspaceStorageConfig struct {
	Backend string `yaml:"backend"`
}

// applyDefaults fills an omitted Backend with the local default. It is idempotent.
func (w *WorkspaceStorageConfig) applyDefaults() {
	if strings.TrimSpace(w.Backend) == "" {
		w.Backend = defaultWorkspaceBackend
	} else {
		w.Backend = strings.ToLower(strings.TrimSpace(w.Backend))
	}
}

// IsShared reports whether the workspace runs in shared-POSIX-filesystem mode,
// i.e. storage.workspace.backend == "shared".
func (w WorkspaceStorageConfig) IsShared() bool {
	return w.Backend == WorkspaceBackendShared
}

// validate rejects an unrecognized Backend value. The set is local|shared; any
// other value fails fast at load time rather than silently behaving as local.
func (w WorkspaceStorageConfig) validate() error {
	switch w.Backend {
	case WorkspaceBackendLocal, WorkspaceBackendShared:
		return nil
	default:
		return fmt.Errorf("storage.workspace.backend: unsupported value %q (want local|shared)", w.Backend)
	}
}

// LandlockConfig holds the process-level Landlock sandbox directory policy (see
// the sandbox-directory-configuration spec). Enabled defaults to true via the
// *bool nil→enabled pattern (matching PipToolConfig.Network): omitting the block
// preserves the historical landlock-protected behavior, while an explicit false
// skips ApplyLandlock entirely (warning-only). SystemReadOnly is the
// stat-guarded read-only system baseline; ExtraReadWrite / ExtraReadOnly are
// additional process-level RW/RO directories. All three lists default to the
// pre-configurability literals so an omitted block is byte-for-byte equivalent.
//
// The process RW/RO application directories ({data-dir}/data, {data-dir}/logs,
// {data-dir}/skills and {data-dir}/tools) are derived from -d and are NOT
// configurable here (design non-goal); this block only adds to them.
type LandlockConfig struct {
	Enabled        *bool    `yaml:"enabled"`
	SystemReadOnly []string `yaml:"system_read_only"`
	ExtraReadWrite []string `yaml:"extra_read_write"`
	ExtraReadOnly  []string `yaml:"extra_read_only"`
}

// IsEnabled reports whether landlock should be applied. It defaults to true when
// Enabled is unset, preserving the historical landlock-protected behavior; an
// explicit false opts out (ApplyLandlock is skipped with a warning).
func (l LandlockConfig) IsEnabled() bool {
	if l.Enabled == nil {
		return true
	}
	return *l.Enabled
}

// DefaultLandlockSystemReadOnly is the default system read-only baseline for the
// process-level Landlock restriction, mirroring the pre-configurability literal
// in landlock_linux.go. It includes /proc because the process-scope restriction
// needs proc readable.
func DefaultLandlockSystemReadOnly() []string {
	return []string{"/etc", "/usr", "/bin", "/lib", "/lib64", "/proc"}
}

// applyDefaults fills an omitted SystemReadOnly with the default baseline. It is
// idempotent. Enabled is intentionally left untouched: IsEnabled handles the
// nil→true default so an explicit false survives a round-trip.
func (l *LandlockConfig) applyDefaults() {
	if len(l.SystemReadOnly) == 0 {
		l.SystemReadOnly = DefaultLandlockSystemReadOnly()
	}
}

// validate enforces the landlock config-shape guards (see the
// sandbox-directory-configuration spec, "配置校验守卫"): every SystemReadOnly /
// ExtraReadOnly / ExtraReadWrite entry must be an absolute path, and
// ExtraReadWrite must not be "/" (too broad). The "≥1 effective RW dir" guard
// (2.1) depends on the runtime-derived dirs and is enforced in setupRuntime via
// ValidateLandlockRW.
func (l LandlockConfig) validate() error {
	for i, d := range l.SystemReadOnly {
		if !isAbs(d) {
			return fmt.Errorf("landlock.system_read_only[%d] %q: must be an absolute path", i, d)
		}
	}
	for i, d := range l.ExtraReadOnly {
		if !isAbs(d) {
			return fmt.Errorf("landlock.extra_read_only[%d] %q: must be an absolute path", i, d)
		}
	}
	for i, d := range l.ExtraReadWrite {
		if !isAbs(d) {
			return fmt.Errorf("landlock.extra_read_write[%d] %q: must be an absolute path", i, d)
		}
		if d == "/" {
			return fmt.Errorf("landlock.extra_read_write[%d]: %q is too broad", i, d)
		}
	}
	return nil
}

// ValidateLandlockRW enforces guard 2.1: when landlock is enabled, the effective
// read-write directory set (defaultRWDirs as derived by setupRuntime from -d,
// plus extraRWDirs) must be non-empty. It preserves applyLandlock's existing
// "≥1 RW directory" invariant. This is a startup-time check (called from
// setupRuntime) because defaultRWDirs are resolved from -d only after config
// load, so it cannot be evaluated inside Config.validate.
func ValidateLandlockRW(enabled bool, defaultRWDirs, extraRWDirs []string) error {
	if !enabled {
		return nil
	}
	for _, d := range defaultRWDirs {
		if strings.TrimSpace(d) != "" {
			return nil
		}
	}
	for _, d := range extraRWDirs {
		if strings.TrimSpace(d) != "" {
			return nil
		}
	}
	return fmt.Errorf("landlock: enabled but no read-write directory is configured")
}

// isAbs reports whether p is an absolute path (starts with '/'). The configured
// paths are Linux sandbox/landlock paths, so a leading slash is the definition
// of absolute; this deliberately avoids platform-specific filepath.IsAbs
// semantics so the behavior is identical when tests run on macOS/Windows.
func isAbs(p string) bool {
	return strings.HasPrefix(p, "/")
}

// OnlyOfficeConfig holds the server-side OnlyOffice DocumentServer integration
// settings. The editor-config signing endpoint signs DocEditor configs with
// Secret (HS256); it must match the DocumentServer local.json secret. ServerURL
// is the browser-facing origin the editor loads api.js from AND the host
// allowlist base for callback result URLs. InternalBackend is the origin the
// DocumentServer container uses to reach the blowball backend (for document.url
// and callbackUrl). VersionServiceURL is the base URL of the external office-vers
// service that the historical-version view config endpoint
// (.../onlyoffice-version-config) points document.url at. All four expand ${VAR}
// like every other config value; when Secret is empty the editor endpoints return
// 503 instead of signing an unverifiable config, and the version-view endpoint
// additionally requires VersionServiceURL.
type OnlyOfficeConfig struct {
	Secret            string `yaml:"secret"`
	ServerURL         string `yaml:"server_url"`
	InternalBackend   string `yaml:"internal_backend"`
	VersionServiceURL string `yaml:"version_service_url"`
}

// applyDefaults fills the OnlyOffice ServerURL default when omitted. The browser
// loads api.js from this origin, so a sensible local default keeps a dev
// DocumentServer working without explicit config.
func (o *OnlyOfficeConfig) applyDefaults() {
	if strings.TrimSpace(o.ServerURL) == "" {
		o.ServerURL = "http://localhost"
	}
}

// DefaultServerPort is the listen port used for the api and all roles when
// server.port is omitted. It matches the value documented in config.example.yaml.
const DefaultServerPort = 8080

// DefaultAgentPort is the listen port used for the agent role when
// server.agent_port is omitted. The agent role runs on its own listener so it
// can be operated independently of the api role (see the service-roles spec).
const DefaultAgentPort = 8081

// ServerConfig holds HTTP server settings.
//
// Port is the listener for the api role and the all role (the all role serves
// the full route set on a single listener). AgentPort is the listener for the
// agent role; it is ignored unless the process is started with --role agent.
type ServerConfig struct {
	Port      int `yaml:"port"`
	AgentPort int `yaml:"agent_port"`
}

// applyDefaults fills zero-valued server fields with the role-aware defaults:
// Port → DefaultServerPort, AgentPort → DefaultAgentPort. It is idempotent.
func (s *ServerConfig) applyDefaults() {
	if s.Port == 0 {
		s.Port = DefaultServerPort
	}
	if s.AgentPort == 0 {
		s.AgentPort = DefaultAgentPort
	}
}

// OpenAIConfig holds OpenAI API client settings.
type OpenAIConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// MySQLConfig holds MySQL connection settings.
type MySQLConfig struct {
	DSN      string `yaml:"dsn"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// JWTConfig holds JWT signing settings.
type JWTConfig struct {
	Secret string `yaml:"secret"`
	Expire string `yaml:"expire"`
}

// AuthConfig holds login policy. PasswordRequired gates whether Login verifies
// the supplied password against the stored bcrypt hash. It is a pointer so an
// unset value can default to "required": omitting the key preserves the
// historical password-based behavior, while an explicit false enables
// passwordless login (any seeded, active user logs in by username alone).
type AuthConfig struct {
	PasswordRequired *bool `yaml:"password_required"`
}

// IsPasswordRequired reports whether login must verify a password. It defaults
// to true when auth.password_required is omitted, preserving the password-based
// default; an explicit false opts into passwordless login.
func (a AuthConfig) IsPasswordRequired() bool {
	if a.PasswordRequired == nil {
		return true
	}
	return *a.PasswordRequired
}

// ParseDuration resolves the configured expire duration. The value may be a
// plain Go time.Duration string (e.g. "24h", "30m") or a short form with a
// trailing unit suffix d/w/h/m (e.g. "7d", "2w", "30m"). Unsupported values
// fall back to time.ParseDuration.
func (j JWTConfig) ParseDuration() (time.Duration, error) {
	raw := strings.TrimSpace(j.Expire)
	if raw == "" {
		return 0, fmt.Errorf("jwt.expire is empty")
	}
	return parseDuration(raw)
}

// AgentMCPConfig holds per-agent MCP server and tool allowlists.
type AgentMCPConfig struct {
	Servers []AgentMCPServerConfig `yaml:"servers"`
}

// AgentMCPServerConfig declares one allowed MCP server and the tools from it
// the agent may use. Tools ["*"] allows every tool discovered from that server.
type AgentMCPServerConfig struct {
	Name  string   `yaml:"name"`
	Tools []string `yaml:"tools"`
}

// AgentRetryConfig is the per-agent transient-error retry policy for sub-agent
// dispatch (capability C). It governs how Confucius retries a sub-agent's LLM
// call when it fails with a transient error (429/5xx/timeout), subject to a
// per-turn token budget and per-agent idempotency (Liang default-enabled /
// Chongzhi default-disabled — a side-effecting agent is only retried before it
// has executed any tool_call). Zero-value fields are filled by
// applyRetryDefaults.
type AgentRetryConfig struct {
	Enabled        bool          `yaml:"enabled"`
	MaxAttempts    int           `yaml:"max_attempts"`    // total attempts including the first; 0 -> default
	InitialBackoff time.Duration `yaml:"initial_backoff"` // first retry delay; 0 -> default
	MaxBackoff     time.Duration `yaml:"max_backoff"`     // backoff cap; 0 -> default
	BudgetTokens   int           `yaml:"budget_tokens"`   // per-turn retry token cap; 0 -> no budget
}

// Default retry backoff parameters (design Open Question, task 7.1): a small
// fixed budget that recovers from a transient blip without amplifying cost on
// a sustained outage. Confirmed values; tune with real 429 data.
const (
	defaultRetryMaxAttempts    = 2
	defaultRetryInitialBackoff = 500 * time.Millisecond
	defaultRetryMaxBackoff     = 4 * time.Second
)

// DefaultRetryMaxAttempts / DefaultRetryInitialBackoff / DefaultRetryMaxBackoff
// expose the retry defaults for callers outside the config package (the agent
// retry wrapper uses them when a policy omits the fields). They mirror the
// private constants above.
func DefaultRetryMaxAttempts() int              { return defaultRetryMaxAttempts }
func DefaultRetryInitialBackoff() time.Duration { return defaultRetryInitialBackoff }
func DefaultRetryMaxBackoff() time.Duration     { return defaultRetryMaxBackoff }

// AgentConfig describes a single agent's runtime settings.
type AgentConfig struct {
	Name            string         `yaml:"name"`
	Model           string         `yaml:"model"`
	SystemPrompt    string         `yaml:"system_prompt"`
	MaxTokens       int            `yaml:"max_tokens"`
	Tools           []string       `yaml:"tools"`
	MCP             AgentMCPConfig `yaml:"mcp"`
	Skills          []string       `yaml:"skills"`
	Thinking        bool           `yaml:"thinking"`
	ReasoningEffort string         `yaml:"reasoning_effort"`
	// OutputSchema is an optional raw JSON Schema (string form) that, when set,
	// makes the sub-agent enable OpenAI structured output
	// (response_format: json_schema) on its FINAL tool-calling round (the
	// round with no tool_calls, finish_reason=stop). This constrains the
	// content returned to the parent to conform to the schema. Only meaningful
	// for sub-agents (Liang); Confucius never sets it and Chongzhi's output is
	// file changes, not structured text. When Thinking is true (reasoning
	// models), structured output is incompatible, so validate() rejects the
	// combination unless the agent is willing to degrade to a prompt-only
	// constraint — enforced by rejecting Thinking+OutputSchema outright here
	// (the spec's model-gate: only the prompt-only degradation path is
	// allowed, and that path is driven purely by prompt text, not this field,
	// so a reasoning agent MUST NOT set output_schema).
	//
	// Decision (task 6.1, design Open Question): the schema is inlined as a
	// YAML multi-line string rather than referenced via output_schema_file —
	// single-file readability wins for the small Liang schemas; split to a file
	// only if a schema grows large.
	OutputSchema string `yaml:"output_schema"`
	// Retry is the per-agent transient-error retry policy (capability C).
	// Defaults are applied per-agent by applyRetryDefaults (Liang retry enabled,
	// Chongzhi disabled).
	Retry AgentRetryConfig `yaml:"retry"`
}

// AgentsConfig holds the three blowball agents.
type AgentsConfig struct {
	Confucius AgentConfig `yaml:"confucius"`
	Chongzhi  AgentConfig `yaml:"chongzhi"`
	Liang     AgentConfig `yaml:"liang"`
}

// validate checks every agent's MCP server references point to a declared
// global MCP server and that reasoning_effort values are valid. Tool and skill
// existence are validated later once the remote tool list and skill directories
// are known.
func (a *AgentsConfig) validate(serverNames map[string]struct{}) error {
	for _, name := range []string{"confucius", "chongzhi", "liang"} {
		var cfg *AgentConfig
		switch name {
		case "confucius":
			cfg = &a.Confucius
		case "chongzhi":
			cfg = &a.Chongzhi
		case "liang":
			cfg = &a.Liang
		}
		if cfg.Thinking {
			if cfg.ReasoningEffort == "" {
				cfg.ReasoningEffort = "medium"
			}
			if cfg.ReasoningEffort != "low" && cfg.ReasoningEffort != "medium" && cfg.ReasoningEffort != "high" && cfg.ReasoningEffort != "xhigh" && cfg.ReasoningEffort != "max" {
				return fmt.Errorf("agents.%s.reasoning_effort: invalid value %q (must be low, medium, high, xhigh or max)", name, cfg.ReasoningEffort)
			}
		} else if cfg.ReasoningEffort != "" {
			return fmt.Errorf("agents.%s.reasoning_effort: cannot be set when thinking is disabled", name)
		}
		// Structured output model-gate (capability A): thinking (reasoning
		// models) is incompatible with OpenAI structured output. The spec's
		// only allowed degradation path for a reasoning model is a prompt-only
		// constraint, which is driven purely by system-prompt text and does
		// NOT use OutputSchema. So a reasoning agent MUST NOT set output_schema;
		// reject the contradictory config at load time rather than failing at
		// runtime.
		if cfg.Thinking && strings.TrimSpace(cfg.OutputSchema) != "" {
			return fmt.Errorf("agents.%s: output_schema cannot be set when thinking is enabled (reasoning models do not support structured output; use prompt-only constraints instead)", name)
		}
		// Validate OutputSchema parses as JSON when set (fail fast at load
		// rather than on the final sub-agent round).
		if strings.TrimSpace(cfg.OutputSchema) != "" {
			if !json.Valid([]byte(cfg.OutputSchema)) {
				return fmt.Errorf("agents.%s.output_schema: must be valid JSON", name)
			}
		}
		// Validate retry backoff parameters when retry is enabled.
		if cfg.Retry.Enabled {
			if cfg.Retry.MaxAttempts < 0 {
				return fmt.Errorf("agents.%s.retry.max_attempts: must be >= 0", name)
			}
			if cfg.Retry.InitialBackoff < 0 || cfg.Retry.MaxBackoff < 0 {
				return fmt.Errorf("agents.%s.retry: backoff durations must be >= 0", name)
			}
			if cfg.Retry.MaxBackoff > 0 && cfg.Retry.InitialBackoff > cfg.Retry.MaxBackoff {
				return fmt.Errorf("agents.%s.retry: initial_backoff must be <= max_backoff", name)
			}
		}
		for i, s := range cfg.MCP.Servers {
			if strings.TrimSpace(s.Name) == "" {
				return fmt.Errorf("agents.%s.mcp.servers[%d]: name must be non-empty", name, i)
			}
			if _, ok := serverNames[s.Name]; !ok {
				return fmt.Errorf("agents.%s.mcp.servers[%d]: unknown mcp server %q", name, i, s.Name)
			}
		}
	}
	return nil
}

// XizhiToolConfig is the enabled flag for a single Xizhi tool.
type XizhiToolConfig struct {
	Enabled bool `yaml:"enabled"`
}

// XizhiConfig groups the Xizhi workspace-scoped file tools.
type XizhiConfig struct {
	Read      XizhiToolConfig `yaml:"read"`
	Write     XizhiToolConfig `yaml:"write"`
	Modify    XizhiToolConfig `yaml:"modify"`
	ListFiles XizhiToolConfig `yaml:"list_files"`
	Tree      XizhiToolConfig `yaml:"tree"`
	GlobFiles XizhiToolConfig `yaml:"glob_files"`
	Delete   XizhiToolConfig `yaml:"delete"`
}

// WebfetchConfig holds the process-level webfetch tool settings.
type WebfetchConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Timeout      time.Duration `yaml:"timeout"`
	MaxRedirects int           `yaml:"max_redirects"`
}

// UserMCPConfig holds per-user MCP tool settings. Per-user MCP activates
// automatically when an agent lists an mcp_* tool; the configurable knobs are
// the connect handshake timeout and the total per-call timeout. Zero values
// fall back to package defaults (connect 5s, total 10s).
type UserMCPConfig struct {
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	CallTimeout    time.Duration `yaml:"call_timeout"`
}

// ExecutorToolConfig holds the per-tool settings for bash/python executors.
type ExecutorToolConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Timeout            time.Duration `yaml:"timeout"`
	MaxOutputBytes     int           `yaml:"max_output_bytes"`
	AllowedEnvPatterns []string      `yaml:"allowed_env_patterns"`
	Network            bool          `yaml:"network"`
}

// ExecutorConfig groups the sandboxed command execution tools.
type ExecutorConfig struct {
	Bash    ExecutorToolConfig    `yaml:"bash"`
	Python  ExecutorToolConfig    `yaml:"python"`
	Pip     PipToolConfig         `yaml:"pip"`
	Sandbox ExecutorSandboxConfig `yaml:"sandbox"`
}

// MountSpec describes one operator-configured extra mount for the bwrap sandbox.
// Host is the absolute host path; Target is the in-sandbox path (it defaults to
// Host when the config entry omits a target). Entries are parsed once at config
// load so the sandbox runner never touches the raw "host:target" strings.
type MountSpec struct {
	Host   string
	Target string
}

// ExecutorSandboxConfig holds the per-command bwrap sandbox directory policy
// (see the sandbox-directory-configuration spec). SystemReadOnly is the
// stat-guarded read-only system baseline (no /proc: bwrap synthesizes /proc via
// --proc). ExtraReadOnly / ExtraReadWrite are operator data-set and
// writable-cache mounts supporting the "host" or "host:target" forms, parsed at
// load time into the *Mounts fields. Defaults reproduce the pre-configurability
// literals so an omitted block is byte-for-byte equivalent.
type ExecutorSandboxConfig struct {
	SystemReadOnly       []string    `yaml:"system_read_only"`
	ExtraReadOnly        []string    `yaml:"extra_read_only"`
	ExtraReadWrite       []string    `yaml:"extra_read_write"`
	ExtraReadOnlyMounts  []MountSpec `yaml:"-"`
	ExtraReadWriteMounts []MountSpec `yaml:"-"`
}

// DefaultExecutorSystemReadOnly is the default system read-only baseline for the
// bwrap sandbox, mirroring the pre-configurability literal in bwrap.go (no
// /proc: bwrap synthesizes /proc itself with --proc).
func DefaultExecutorSystemReadOnly() []string {
	return []string{"/usr", "/bin", "/lib", "/lib64", "/etc"}
}

// applyDefaults fills an omitted SystemReadOnly with the default baseline. It is
// idempotent.
func (s *ExecutorSandboxConfig) applyDefaults() {
	if len(s.SystemReadOnly) == 0 {
		s.SystemReadOnly = DefaultExecutorSystemReadOnly()
	}
}

// ParseMounts parses operator extra-mount entries of the form "host" (target
// equals host) or "host:target" (custom in-sandbox path) into MountSpec values.
// Each host MUST be absolute; a relative or empty host is rejected. A
// "host:target" entry with an empty target is rejected. This runs at config load
// (fail-fast): on success the sandbox runner consumes only the parsed MountSpec
// slice, never the raw strings.
func ParseMounts(entries []string) ([]MountSpec, error) {
	out := make([]MountSpec, 0, len(entries))
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, fmt.Errorf("entry[%d]: empty", i)
		}
		host, target := e, e
		if idx := strings.IndexByte(e, ':'); idx >= 0 {
			host = strings.TrimSpace(e[:idx])
			target = strings.TrimSpace(e[idx+1:])
			if target == "" {
				return nil, fmt.Errorf("entry[%d] %q: target after ':' is empty", i, e)
			}
		}
		if host == "" || !isAbs(host) {
			return nil, fmt.Errorf("entry[%d] %q: host must be an absolute path", i, e)
		}
		out = append(out, MountSpec{Host: host, Target: target})
	}
	return out, nil
}

// sandboxInvariantTargets are the fixed bwrap in-sandbox paths that the
// load-bearing invariants (PYTHONPATH /workspace/.pip, --chdir /workspace, the
// synthetic $HOME, $HOME/.local/bin, the skills mounts) depend on. An extra
// mount targeting any of them is rejected (guard 2.4) because it would shadow
// or collide with a fixed path and break sandbox semantics.
var sandboxInvariantTargets = []string{"/workspace", "/home", "/skills", "/tmp", "/proc", "/dev"}

// validate enforces the executor sandbox config-shape guards and resolves the
// extra-mount entries into parsed MountSpecs. SystemReadOnly must be absolute;
// ExtraReadWrite must not be "/"; extra-mount hosts must be absolute (via
// ParseMounts); extra-mount targets must not collide with the fixed invariants
// or a system baseline entry (guard 2.4). On success ExtraReadOnlyMounts /
// ExtraReadWriteMounts are populated for the sandbox runner. It has a pointer
// receiver because it populates those parsed fields.
func (s *ExecutorSandboxConfig) validate() error {
	for i, d := range s.SystemReadOnly {
		if !isAbs(d) {
			return fmt.Errorf("tools.executor.sandbox.system_read_only[%d] %q: must be an absolute path", i, d)
		}
	}
	for i, d := range s.ExtraReadWrite {
		if d == "/" {
			return fmt.Errorf("tools.executor.sandbox.extra_read_write[%d]: %q is too broad", i, d)
		}
	}

	roMounts, err := ParseMounts(s.ExtraReadOnly)
	if err != nil {
		return fmt.Errorf("tools.executor.sandbox.extra_read_only: %w", err)
	}
	rwMounts, err := ParseMounts(s.ExtraReadWrite)
	if err != nil {
		return fmt.Errorf("tools.executor.sandbox.extra_read_write: %w", err)
	}

	// Forbidden target set: the fixed invariants plus the system baseline (an
	// extra mount should not shadow a baseline read-only bind).
	forbidden := make(map[string]struct{}, len(sandboxInvariantTargets)+len(s.SystemReadOnly))
	for _, t := range sandboxInvariantTargets {
		forbidden[t] = struct{}{}
	}
	for _, t := range s.SystemReadOnly {
		forbidden[t] = struct{}{}
	}
	for _, m := range roMounts {
		if _, bad := forbidden[m.Target]; bad {
			return fmt.Errorf("tools.executor.sandbox.extra_read_only: target %q conflicts with a fixed sandbox path or system baseline", m.Target)
		}
	}
	for _, m := range rwMounts {
		if _, bad := forbidden[m.Target]; bad {
			return fmt.Errorf("tools.executor.sandbox.extra_read_write: target %q conflicts with a fixed sandbox path or system baseline", m.Target)
		}
	}

	s.ExtraReadOnlyMounts = roMounts
	s.ExtraReadWriteMounts = rwMounts
	return nil
}

// DefaultExecutorToolConfig returns the recommended defaults for an executor
// tool. It is used when a tool block is omitted or fields are zero-valued.
func DefaultExecutorToolConfig() ExecutorToolConfig {
	return ExecutorToolConfig{
		Enabled:            false,
		Timeout:            30 * time.Second,
		MaxOutputBytes:     65536,
		AllowedEnvPatterns: []string{"PATH", "HOME", "LANG", "USER", "TERM", "PYTHON*"},
		Network:            false,
	}
}

// PipToolConfig holds the per-tool settings for the pip_install executor tool.
// It mirrors ExecutorToolConfig but defaults network to true because pip
// requires network access in most deployments.
type PipToolConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Timeout            time.Duration `yaml:"timeout"`
	MaxOutputBytes     int           `yaml:"max_output_bytes"`
	AllowedEnvPatterns []string      `yaml:"allowed_env_patterns"`
	Network            *bool         `yaml:"network"`
	IndexURL           string        `yaml:"index_url"`
	ExtraIndexURLs     []string      `yaml:"extra_index_urls"`
	TrustedHosts       []string      `yaml:"trusted_hosts"`
}

// DefaultPipToolConfig returns the recommended defaults for pip_install.
func DefaultPipToolConfig() PipToolConfig {
	return PipToolConfig{
		Enabled:            false,
		Timeout:            120 * time.Second,
		MaxOutputBytes:     65536,
		AllowedEnvPatterns: []string{"PATH", "HOME", "LANG", "USER", "TERM", "PYTHON*"},
		Network:            boolPtr(true),
	}
}

// NetworkEnabled reports whether pip_install should have network access,
// defaulting to true when the config leaves the field unset.
func (p *PipToolConfig) NetworkEnabled() bool {
	if p.Network == nil {
		return true
	}
	return *p.Network
}

// ToExecutorToolConfig converts the pip-specific config into the generic
// executor tool shape used by the sandbox runner.
func (p *PipToolConfig) ToExecutorToolConfig() ExecutorToolConfig {
	return ExecutorToolConfig{
		Enabled:            p.Enabled,
		Timeout:            p.Timeout,
		MaxOutputBytes:     p.MaxOutputBytes,
		AllowedEnvPatterns: p.AllowedEnvPatterns,
		Network:            p.NetworkEnabled(),
	}
}

// ApplyDefaults fills zero-valued executor fields with the recommended defaults.
// A negative MaxOutputBytes is left as-is so the caller can reject it;
// a zero value is replaced with the default.
func (e *ExecutorToolConfig) ApplyDefaults() {
	def := DefaultExecutorToolConfig()
	if e.Timeout == 0 {
		e.Timeout = def.Timeout
	}
	if e.MaxOutputBytes == 0 {
		e.MaxOutputBytes = def.MaxOutputBytes
	}
	if len(e.AllowedEnvPatterns) == 0 {
		e.AllowedEnvPatterns = def.AllowedEnvPatterns
	}
}

// ApplyDefaults fills zero-valued pip fields with the recommended defaults.
func (p *PipToolConfig) ApplyDefaults() {
	def := DefaultPipToolConfig()
	if p.Timeout == 0 {
		p.Timeout = def.Timeout
	}
	if p.MaxOutputBytes == 0 {
		p.MaxOutputBytes = def.MaxOutputBytes
	}
	if len(p.AllowedEnvPatterns) == 0 {
		p.AllowedEnvPatterns = def.AllowedEnvPatterns
	}
	if p.Network == nil {
		p.Network = def.Network
	}
}

func boolPtr(b bool) *bool {
	return &b
}

// ToolsConfig groups all tool configuration.
type ToolsConfig struct {
	Xizhi    XizhiConfig    `yaml:"xizhi"`
	Webfetch WebfetchConfig `yaml:"webfetch"`
	Executor ExecutorConfig `yaml:"executor"`
	UserMCP  UserMCPConfig  `yaml:"user_mcp"`
}

// MCPConfig holds external MCP server configuration.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig describes a single external MCP server.
type MCPServerConfig struct {
	Name        string            `yaml:"name"`
	Transport   string            `yaml:"transport"`
	URL         string            `yaml:"url"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Env         map[string]string `yaml:"env"`
	Headers     map[string]string `yaml:"headers"`
	Timeout     time.Duration     `yaml:"timeout"`
	CallTimeout time.Duration     `yaml:"call_timeout"`
	Reconnect   bool              `yaml:"reconnect"`
	Prefix      string            `yaml:"prefix"`
}

// LoggingConfig holds structured logging settings.
//
// Output selects the log sinks. Recognized values are "stderr", "stdout"
// (console sinks) and "file" (the rotated file sink under the runtime logs
// directory). When Output is empty it defaults to ["stderr", "file"]. Format
// selects the encoder for every enabled sink: "json" (default) or "console".
type LoggingConfig struct {
	Level  string        `yaml:"level"`
	Format string        `yaml:"format"`
	Output []string      `yaml:"output"`
	File   LogFileConfig `yaml:"file"`
}

// LogFileConfig holds lumberjack rotation settings for the file log sink.
// Zero values fall back to the defaults applied in applyDefaults.
type LogFileConfig struct {
	MaxSizeMB  int  `yaml:"max_size_mb"`
	MaxBackups int  `yaml:"max_backups"`
	MaxAgeDays int  `yaml:"max_age_days"`
	Compress   bool `yaml:"compress"`
}

// DefaultLogFileConfig returns the lumberjack defaults used when a file sink is
// enabled but the operator omits one or more rotation fields. Compress is left
// false to match the bool zero value (it cannot be auto-defaulted, since false
// is itself a valid explicit choice).
func DefaultLogFileConfig() LogFileConfig {
	return LogFileConfig{
		MaxSizeMB:  100,
		MaxBackups: 7,
		MaxAgeDays: 30,
	}
}

// DefaultLoggingOutput is the sink set used when logging.output is omitted.
var DefaultLoggingOutput = []string{"stderr", "file"}

// applyDefaults fills zero-valued logging fields with the recommended
// defaults: format → "json", output → [stderr, file], and the lumberjack
// rotation defaults. It is idempotent.
func (l *LoggingConfig) applyDefaults() {
	if strings.TrimSpace(l.Format) == "" {
		l.Format = "json"
	}
	if len(l.Output) == 0 {
		l.Output = append([]string(nil), DefaultLoggingOutput...)
	}
	def := DefaultLogFileConfig()
	if l.File.MaxSizeMB == 0 {
		l.File.MaxSizeMB = def.MaxSizeMB
	}
	if l.File.MaxBackups == 0 {
		l.File.MaxBackups = def.MaxBackups
	}
	if l.File.MaxAgeDays == 0 {
		l.File.MaxAgeDays = def.MaxAgeDays
	}
}

// Load reads the YAML config at path, expands ${VAR} / ${VAR:default}
// environment references, unmarshals it into a Config, and validates required
// fields. It returns an error if the file cannot be read, parsed, or fails
// validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	expanded := expandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	cfg.Logging.applyDefaults()
	cfg.OnlyOffice.applyDefaults()
	cfg.Server.applyDefaults()
	cfg.Storage.Workspace.applyDefaults()
	cfg.Landlock.applyDefaults()
	cfg.Tools.Executor.Bash.ApplyDefaults()
	cfg.Tools.Executor.Python.ApplyDefaults()
	cfg.Tools.Executor.Pip.ApplyDefaults()
	cfg.Tools.Executor.Sandbox.applyDefaults()
	// Per-agent retry defaults (capability C): Liang (read-only) defaults to
	// retry-enabled; Chongzhi (side-effecting) defaults to retry-disabled. The
	// zero-value Enabled=false is preserved for Chongzhi (the safe default for
	// a file-writing agent), while Liang's zero value is overridden to enabled.
	cfg.Agents.applyRetryDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate enforces required fields after loading.
func (c *Config) validate() error {
	if strings.TrimSpace(c.JWT.Secret) == "" {
		return fmt.Errorf("config validation error: jwt.secret must be non-empty")
	}
	if strings.TrimSpace(c.MySQL.DSN) == "" {
		return fmt.Errorf("config validation error: mysql.dsn must be non-empty")
	}
	if err := c.Logging.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.Storage.Workspace.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.MCP.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.Agents.validate(c.MCP.serverNames()); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.Landlock.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.Tools.Executor.Sandbox.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	return nil
}

// validate checks the logging format and output sink names. Format must be one
// of json|console; output entries must each be one of stderr|stdout|file.
func (l LoggingConfig) validate() error {
	switch l.Format {
	case "json", "console":
	default:
		return fmt.Errorf("logging.format: unsupported value %q (want json|console)", l.Format)
	}
	for i, sink := range l.Output {
		switch sink {
		case "stderr", "stdout", "file":
		default:
			return fmt.Errorf("logging.output[%d]: unsupported sink %q (want stderr|stdout|file)", i, sink)
		}
	}
	return nil
}

// validate checks every configured MCP server for required fields and
// uniqueness.
func (m MCPConfig) validate() error {
	seen := make(map[string]struct{}, len(m.Servers))
	for i, s := range m.Servers {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("mcp.servers[%d]: name must be non-empty", i)
		}
		if strings.TrimSpace(s.Transport) == "" {
			return fmt.Errorf("mcp.servers[%d]: transport must be non-empty", i)
		}
		switch s.Transport {
		case "sse":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): url is required for sse transport", i, s.Name)
			}
		case "http":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): url is required for http transport", i, s.Name)
			}
		case "stdio":
			if strings.TrimSpace(s.Command) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): command is required for stdio transport", i, s.Name)
			}
		default:
			return fmt.Errorf("mcp.servers[%d] (name=%q): unsupported transport %q", i, s.Name, s.Transport)
		}
		if _, exists := seen[s.Name]; exists {
			return fmt.Errorf("mcp.servers: duplicate server name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// applyRetryDefaults fills per-agent retry defaults: Liang (read-only)
// defaults to retry-enabled; Chongzhi (side-effecting file writes) defaults to
// retry-disabled. Confucius itself is never retried (it is the dispatcher).
// Backoff fields use the package defaults when zero. An agent that explicitly
// sets Retry.Enabled keeps its choice; only the zero value is overridden for
// Liang/Chongzhi. It is idempotent.
func (a *AgentsConfig) applyRetryDefaults() {
	applyOne := func(cfg *AgentConfig, defaultEnabled bool) {
		// Only override Enabled when the agent left the whole Retry block at
		// zero (MaxAttempts==0 && !Enabled && backoffs==0), i.e. the operator
		// did not configure retry at all. This lets an operator explicitly
		// disable Liang retry by setting retry: { enabled: false }.
		if cfg.Retry == (AgentRetryConfig{}) {
			cfg.Retry.Enabled = defaultEnabled
		}
		if cfg.Retry.Enabled {
			if cfg.Retry.MaxAttempts == 0 {
				cfg.Retry.MaxAttempts = defaultRetryMaxAttempts
			}
			if cfg.Retry.InitialBackoff == 0 {
				cfg.Retry.InitialBackoff = defaultRetryInitialBackoff
			}
			if cfg.Retry.MaxBackoff == 0 {
				cfg.Retry.MaxBackoff = defaultRetryMaxBackoff
			}
		}
	}
	applyOne(&a.Liang, true)     // read-only: retry by default
	applyOne(&a.Chongzhi, false) // side-effecting: do not retry by default
}

// serverNames returns the set of declared global MCP server names.
func (m MCPConfig) serverNames() map[string]struct{} {
	out := make(map[string]struct{}, len(m.Servers))
	for _, s := range m.Servers {
		out[s.Name] = struct{}{}
	}
	return out
}

// ValidateAgentMCPTools checks every concrete tool name listed in agent MCP
// configurations against the discovered tools for the referenced server.
// serverTools maps server name to the set of prefixed tool names discovered from
// that server. A wildcard ("*") entry is always valid. The function is intended
// to be called after MCP client registration has populated serverTools.
func (c *Config) ValidateAgentMCPTools(serverTools map[string]map[string]struct{}) error {
	for _, agentName := range []string{"confucius", "chongzhi", "liang"} {
		var cfg AgentConfig
		switch agentName {
		case "confucius":
			cfg = c.Agents.Confucius
		case "chongzhi":
			cfg = c.Agents.Chongzhi
		case "liang":
			cfg = c.Agents.Liang
		}
		for _, s := range cfg.MCP.Servers {
			known, ok := serverTools[s.Name]
			if !ok {
				return fmt.Errorf("agents.%s.mcp.servers: unknown server %q", agentName, s.Name)
			}
			for _, toolName := range s.Tools {
				if toolName == "*" {
					continue
				}
				if _, exists := known[toolName]; !exists {
					return fmt.Errorf("agents.%s.mcp.servers[%q]: unknown tool %q", agentName, s.Name, toolName)
				}
			}
		}
	}
	return nil
}

// ValidateAgentSkills checks every skill name listed in agent configurations
// against the union of global and per-user skill names for the supplied userID.
// The hasSkill function should report whether a skill with the given name
// exists. An empty userID checks only the global skill directory.
func (c *Config) ValidateAgentSkills(userID string, hasSkill func(name, userID string) bool) error {
	for _, agentName := range []string{"confucius", "chongzhi", "liang"} {
		var cfg AgentConfig
		switch agentName {
		case "confucius":
			cfg = c.Agents.Confucius
		case "chongzhi":
			cfg = c.Agents.Chongzhi
		case "liang":
			cfg = c.Agents.Liang
		}
		for _, skillName := range cfg.Skills {
			if !hasSkill(skillName, userID) {
				return fmt.Errorf("agents.%s.skills: unknown skill %q", agentName, skillName)
			}
		}
	}
	return nil
}

// corresponding environment variable values. A reference with a default is
// left as the default when the variable is unset; a reference without a
// default becomes empty when unset, matching os.ExpandEnv semantics while
// adding optional defaults.
func expandEnv(s string) string {
	return envExpander.ReplaceAllStringFunc(s, func(match string) string {
		// Strip surrounding ${ and }.
		inner := match[2 : len(match)-1]
		name := inner
		def := ""
		if i := strings.IndexByte(inner, ':'); i >= 0 {
			name = inner[:i]
			def = inner[i+1:]
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	})
}

var envExpander = regexpMustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:[^}]*)?\}`)
