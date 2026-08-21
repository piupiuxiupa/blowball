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
	Messages   MessagesConfig   `yaml:"messages"`
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

// Default message-flush parameters (message-write-behind capability): the
// ticker is deliberately shorter than llmraw's 5s because the history API is
// sensitive to when a just-sent turn becomes readable from MySQL.
const (
	defaultMessageFlushInterval  = time.Second
	defaultMessageFlushBatchSize = 100
)

// MessagesConfig holds the Redis-first write-behind message persistence knobs
// (the top-level `messages:` block; see the message-write-behind capability).
// FlushInterval is the background flusher's ticker — the upper bound on how
// long a persisted message can sit in the Redis ingest queue before it lands
// in MySQL (and therefore the read-your-writes window of the history
// endpoint). FlushBatchSize caps the records claimed per flush round and
// doubles as the queue-length threshold that triggers an early flush.
// Omitted/zero fields fall back to the defaults; explicit negative values are
// rejected at load time.
type MessagesConfig struct {
	FlushInterval  time.Duration `yaml:"flush_interval"`
	FlushBatchSize int           `yaml:"flush_batch_size"`
}

// applyDefaults fills zero-valued fields with the documented defaults. It is
// idempotent and leaves explicit values (including negatives, which validate
// rejects) untouched.
func (m *MessagesConfig) applyDefaults() {
	if m.FlushInterval == 0 {
		m.FlushInterval = defaultMessageFlushInterval
	}
	if m.FlushBatchSize == 0 {
		m.FlushBatchSize = defaultMessageFlushBatchSize
	}
}

// validate rejects explicit non-positive values. After applyDefaults a zero
// never survives, so this only fires on negatives — a typo, not "use the
// default" (mirrors the agents.<name>.max_rounds convention).
func (m MessagesConfig) validate() error {
	if m.FlushInterval < 0 {
		return fmt.Errorf("messages.flush_interval: must be positive (got %s)", m.FlushInterval)
	}
	if m.FlushBatchSize < 0 {
		return fmt.Errorf("messages.flush_batch_size: must be positive (got %d)", m.FlushBatchSize)
	}
	return nil
}

// LandlockConfig holds the process-level Landlock sandbox directory policy (see
// the sandbox-directory-configuration spec). Enabled defaults to true via the
// *bool nil→enabled pattern: omitting the block preserves the historical
// landlock-protected behavior, while an explicit false
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
	// TitleModel names the model used ONLY for async title generation
	// (model-effort-v2): it is a plain gateway model name, does NOT have to
	// appear in the Models catalog, and defaults to the default catalog entry
	// (see Config.TitleModelName). It replaced the legacy dual-meaning
	// openai.model field, which load now rejects as a residual.
	TitleModel string `yaml:"title_model"`
	// DefaultReasoningEffort is the deployment-level default thinking effort
	// (model-effort-v2): the single configuration source for the effort axis,
	// overridden per request by the `reasoning_effort` parameter. Closed set
	// none|low|medium|high|xhigh|max; unset normalizes to "none"
	// (applyDefaults) preserving the pre-change "thinking off by default"
	// spirit.
	DefaultReasoningEffort string `yaml:"default_reasoning_effort"`
	// StreamIdleTimeout bounds the maximum gap between consecutive SSE frames
	// of a streaming chat call, including time-to-first-frame (the
	// llm-stream-watchdog capability). A stalled stream is aborted with a
	// typed transient-classified error instead of hanging the turn forever.
	// Zero (unset) disables the watchdog entirely (the pre-capability
	// behavior — the tools.timeouts opt-in convention); validate() rejects
	// negatives at load time. Unlike max_context_tokens no raw-value shadow
	// check is needed: yaml.v3 decodes durations via time.ParseDuration,
	// which parses fractions exactly (1.5s → 1500ms) and rejects garbage.
	StreamIdleTimeout time.Duration `yaml:"stream_idle_timeout"`
	// Models is the MANDATORY model catalog (per-request-model-selection,
	// tightened by model-effort-v2): the list of models a request may select
	// via the `model` / `reasoning_effort` body parameters, and the only
	// source of per-turn models — agents no longer carry model fields. All
	// entries share this single gateway's base_url/api_key. An empty catalog
	// fails config load (no implicit single-entry synthesis anymore). Catalog
	// entries must have unique, non-empty names and a positive-integer
	// max_context_tokens (the context-compaction threshold for turns that
	// resolve to that entry).
	Models []ModelCatalogEntry `yaml:"models"`
	// DefaultModel names the catalog entry used when a request omits `model`
	// (and the entry whose window bounds compaction for parameter-less
	// turns). Empty (the default) selects the FIRST catalog entry. Setting it
	// to a name outside the catalog fails validation.
	DefaultModel string `yaml:"default_model"`
	// LengthContinue is the finish_reason=length auto-continuation policy
	// (llm-length-continuation capability): a zero block disables the feature
	// (length keeps terminating the round silently, the pre-capability
	// behavior); any non-zero field enables it with missing siblings
	// defaulted. See LengthContinueConfig.
	LengthContinue LengthContinueConfig `yaml:"length_continue"`
}

// LengthContinueConfig configures the finish_reason=length continuation
// (llm-length-continuation capability). When enabled, a round whose LLM
// response ends length is continued — partial output kept, budget expanded by
// ExpandStep per attempt — up to MaxRetries continuations before the turn
// fails with agent_error length_exhausted. A zero block (unset or all-zero
// fields) disables the feature entirely; negatives are rejected at load.
type LengthContinueConfig struct {
	// ExpandStep is the additive max_tokens increment per continuation
	// attempt: attempt N (0-based) sends cfg.max_tokens + N*ExpandStep.
	// Zero defaults to DefaultLengthContinueStep() once the block is enabled.
	ExpandStep int `yaml:"expand_step"`
	// MaxRetries is the number of CONTINUATIONS allowed per round (3 → 4
	// total attempts). Zero defaults to DefaultLengthContinueRetries() once
	// the block is enabled. Continuation attempts do not consume max_rounds.
	MaxRetries int `yaml:"max_retries"`
}

// Enabled reports whether the continuation feature is on: any non-zero field.
// A zero block means byte-for-byte pre-capability behavior.
func (l LengthContinueConfig) Enabled() bool {
	return l.ExpandStep > 0 || l.MaxRetries > 0
}

// Resolve returns the normalized (expandStep, maxRetries) pair for an enabled
// block, substituting defaults for zero siblings. Callers must consult
// Enabled() first; a disabled block resolves to (0, 0).
func (l LengthContinueConfig) Resolve() (expandStep, maxRetries int) {
	if !l.Enabled() {
		return 0, 0
	}
	step, retries := l.ExpandStep, l.MaxRetries
	if step <= 0 {
		step = DefaultLengthContinueStep()
	}
	if retries <= 0 {
		retries = DefaultLengthContinueRetries()
	}
	return step, retries
}

// Default continuation parameters (design D9): a step matching the common
// agents.*.max_tokens (8192) and the explored retry bound of 3 continuations.
func DefaultLengthContinueStep() int    { return 8192 }
func DefaultLengthContinueRetries() int { return 3 }

// ModelCatalogEntry is one selectable model in the openai.models catalog
// (per-request-model-selection). The entry carries only the per-model
// dimensions a request can select on: the model name (what the request sends
// and what the agents run), its context window in tokens (drives the
// context-compaction threshold for turns that resolve to this model), and
// whether the model supports reasoning — a capability marker that decides the
// entry's wire family (thinking entries always send reasoning_effort, literal
// none included, plus max_completion_tokens) and gates request
// reasoning_effort values (a non-thinking entry only accepts none, with the
// deployment default clamped to none + WARN).
type ModelCatalogEntry struct {
	Name             string `yaml:"name"`
	MaxContextTokens int    `yaml:"max_context_tokens"`
	Thinking         bool   `yaml:"thinking"`
}

// reasoningEfforts is the closed set of legal openai.default_reasoning_effort
// (and request reasoning_effort) values.
var reasoningEfforts = map[string]bool{
	"none": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// applyDefaults normalizes an omitted DefaultReasoningEffort to "none". It is
// idempotent and leaves explicit values (valid or not) untouched — validate
// rejects illegal ones.
func (o *OpenAIConfig) applyDefaults() {
	if strings.TrimSpace(o.DefaultReasoningEffort) == "" {
		o.DefaultReasoningEffort = "none"
	}
}

// FindModelCatalogEntry returns the explicit catalog entry with the given
// name, ok=false when the catalog is unconfigured or holds no such entry.
func (o OpenAIConfig) FindModelCatalogEntry(name string) (ModelCatalogEntry, bool) {
	for _, m := range o.Models {
		if m.Name == name {
			return m, true
		}
	}
	return ModelCatalogEntry{}, false
}

// validate enforces the model-effort-v2 config shape. The openai.models
// catalog is MANDATORY (≥1 entry — an empty catalog fails load instead of
// synthesizing an implicit one). Entries must have unique, non-empty names
// and a positive-integer max_context_tokens (the compaction threshold;
// fractional values are caught by the raw-value shadow check in Load —
// yaml.v3 silently truncates them — negatives decode exactly and are
// rejected here). default_model (when set) must name a catalog entry, and
// default_reasoning_effort must be inside the closed set
// none|low|medium|high|xhigh|max (applyDefaults already normalized an empty
// value to none). A negative StreamIdleTimeout is a typo: zero is the
// documented off switch, so nothing legitimate decodes negative.
func (o OpenAIConfig) validate() error {
	if o.StreamIdleTimeout < 0 {
		return fmt.Errorf("openai.stream_idle_timeout: must be a positive duration or 0 (0 disables the stream idle watchdog; got %s)", o.StreamIdleTimeout)
	}
	if o.LengthContinue.ExpandStep < 0 || o.LengthContinue.MaxRetries < 0 {
		return fmt.Errorf("openai.length_continue: expand_step and max_retries must be positive integers or 0 (0/unset disables length continuation; got expand_step=%d, max_retries=%d)", o.LengthContinue.ExpandStep, o.LengthContinue.MaxRetries)
	}
	if len(o.Models) == 0 {
		return fmt.Errorf("openai.models: the model catalog is required (model-effort-v2): configure at least one {name, max_context_tokens, thinking} entry")
	}
	seen := make(map[string]struct{}, len(o.Models))
	for i, m := range o.Models {
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("openai.models[%d].name: must be non-empty", i)
		}
		if _, dup := seen[m.Name]; dup {
			return fmt.Errorf("openai.models[%d].name: duplicate model name %q (catalog names must be unique)", i, m.Name)
		}
		seen[m.Name] = struct{}{}
		if m.MaxContextTokens <= 0 {
			return fmt.Errorf("openai.models[%d].max_context_tokens: must be a positive integer (got %d)", i, m.MaxContextTokens)
		}
	}
	if o.DefaultModel != "" {
		if _, ok := seen[o.DefaultModel]; !ok {
			return fmt.Errorf("openai.default_model: %q is not an openai.models catalog entry", o.DefaultModel)
		}
	}
	if !reasoningEfforts[o.DefaultReasoningEffort] {
		return fmt.Errorf("openai.default_reasoning_effort: invalid value %q (must be none, low, medium, high, xhigh or max)", o.DefaultReasoningEffort)
	}
	return nil
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

// defaultAgentMaxRounds is the per-agent tool-calling loop cap applied when an
// agent omits max_rounds (or sets it <= 0). 100 reproduces the prior
// hard-coded constants exactly (zero behavior change); operators tune the cap
// per agent via agents.<name>.max_rounds.
const defaultAgentMaxRounds = 100

// DefaultAgentMaxRounds exposes the agent round-cap default for callers
// outside the config package (the agent constructors apply it when an agent
// omits max_rounds). Mirrors the private constant above.
func DefaultAgentMaxRounds() int { return defaultAgentMaxRounds }

// AgentConfig describes a single agent's runtime settings. It carries ONLY
// the agent's capability surface — prompt, quotas, tools, skills, round cap,
// structured output, retry (model-effort-v2 removed the model/thinking/
// reasoning_effort fields: the model and effort are turn-level attributes
// resolved from the openai.models catalog + openai.default_reasoning_effort +
// request parameters, and load rejects the removed fields as residuals).
type AgentConfig struct {
	Name         string         `yaml:"name"`
	SystemPrompt string         `yaml:"system_prompt"`
	MaxTokens    int            `yaml:"max_tokens"`
	Tools        []string       `yaml:"tools"`
	MCP          AgentMCPConfig `yaml:"mcp"`
	Skills       []string       `yaml:"skills"`
	// MaxRounds bounds the agent's tool-calling loop — the number of LLM
	// rounds the loop runs before terminating. When the model keeps emitting
	// tool_calls without converging, the loop stops at MaxRounds and the agent
	// runs one tool-disabled wrap-up round to synthesize a final answer (see
	// the agent-orchestration spec, "Agent tool-calling loop round cap and
	// graceful termination"). 0 / unset falls back to DefaultAgentMaxRounds()
	// at agent construction; validate() rejects negative values.
	MaxRounds int `yaml:"max_rounds"`
	// OutputSchema is an optional raw JSON Schema (string form) that, when set,
	// makes the sub-agent enable OpenAI structured output
	// (response_format: json_schema) on its FINAL tool-calling round (the
	// round with no tool_calls, finish_reason=stop). This constrains the
	// content returned to the parent to conform to the schema. Only meaningful
	// for sub-agents (Liang); Confucius never sets it and Chongzhi's output is
	// file changes, not structured text. Structured output is incompatible
	// with reasoning (effort != none), so Config.validate fails the load when
	// any output_schema agent coexists with a non-none
	// openai.default_reasoning_effort (model-effort-v2's config-level gate),
	// and the handler 400s requests whose resolved effort is not none.
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
// global MCP server. Tool and skill existence are validated later once the
// remote tool list and skill directories are known. (model-effort-v2 removed
// the per-agent thinking/effort validation — those fields no longer exist;
// the output_schema × reasoning gate now lives in Config.validate against
// openai.default_reasoning_effort.)
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
		// MaxRounds: negative is a typo, not "use default" — reject it. 0 is
		// valid and means "use the default" (applied at agent construction, not
		// here, matching how retry defaults are applied per-agent).
		if cfg.MaxRounds < 0 {
			return fmt.Errorf("agents.%s.max_rounds: must be >= 0 (0 means use the default)", name)
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
	Grep      XizhiToolConfig `yaml:"grep"`
	Delete    XizhiToolConfig `yaml:"delete"`
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

// ExecutorToolConfig holds the per-tool settings for the bash executor. Network
// is a *bool so an omitted value defaults to ENABLED (true) — bash ships with
// network on so pip-via-bash (python3 -m pip install) reaches PyPI out of the
// box; operators set network: false to tighten. Use NetworkEnabled() to read the
// effective value (nil → true).
type ExecutorToolConfig struct {
	Enabled            bool          `yaml:"enabled"`
	Timeout            time.Duration `yaml:"timeout"`
	MaxOutputBytes     int           `yaml:"max_output_bytes"`
	AllowedEnvPatterns []string      `yaml:"allowed_env_patterns"`
	// Env is the operator-defined environment-literal map (KEY: value) injected
	// into every bash sandbox as the middle layer of the three-tier env
	// construction: host allowlist (filterEnv, lowest) < Env literals (middle) <
	// forced invariants (HOME, PATH prepend, PYTHONPATH prepend, highest). An Env
	// entry overrides a same-named host allowlist variable and is itself
	// overridden by the forced invariants. Values are subject to the global
	// ${VAR} / ${VAR:default} expansion applied to the whole YAML document at
	// load, so secrets may reference the host environment rather than being
	// hard-coded. HOME is a reserved key (rejected by validate); key names must
	// match ^[A-Za-z_][A-Za-z0-9_]*$. Omitted/empty → zero behavior change.
	Env     map[string]string `yaml:"env"`
	Network *bool             `yaml:"network"`
}

// ExecutorConfig groups the sandboxed command execution tools. Only the bash
// executor remains; the dedicated python/pip_install executors were removed
// (Python code and pip installs run via bash). The persistent PYTHONPATH bridge
// (/workspace/.pip) is still injected into every bash sandbox so pip-via-bash
// installs remain importable by later python3 invocations.
type ExecutorConfig struct {
	Bash    ExecutorToolConfig    `yaml:"bash"`
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

// DefaultExecutorToolConfig returns the recommended defaults for the bash
// executor. It is used when the tool block is omitted or fields are zero-valued.
// Network defaults to true so pip-via-bash (python3 -m pip install) can reach
// PyPI out of the box; operators may set bash.network: false to tighten.
func DefaultExecutorToolConfig() ExecutorToolConfig {
	return ExecutorToolConfig{
		Enabled:            false,
		Timeout:            30 * time.Second,
		MaxOutputBytes:     65536,
		AllowedEnvPatterns: []string{"PATH", "HOME", "LANG", "USER", "TERM", "PYTHON*"},
		Network:            BoolPtr(true),
	}
}

// ApplyDefaults fills zero-valued executor fields with the recommended defaults.
// A negative MaxOutputBytes is left as-is so the caller can reject it;
// a zero value is replaced with the default. Network is a *bool: an unset (nil)
// value is filled with the default (true) so the effective value round-trips;
// an explicit false survives unchanged.
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
	if e.Network == nil {
		e.Network = def.Network
	}
}

// NetworkEnabled reports the effective network policy for the bash sandbox:
// true when Network is unset (the default, so pip-via-bash reaches PyPI) or
// explicitly true; false only when explicitly set to false.
func (e ExecutorToolConfig) NetworkEnabled() bool {
	if e.Network == nil {
		return true
	}
	return *e.Network
}

// envKeyNameRe matches a legal environment-variable name: an ASCII letter or
// underscore followed by letters, digits, or underscores. It is the fail-fast
// guard for tools.executor.bash.env keys (design D5).
var envKeyNameRe = regexpMustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validate enforces the operator env-literal guards on
// tools.executor.bash.env (fail-fast, in the ParseMounts style): HOME is a
// reserved key (the forced invariant layer always wins it, so an operator value
// would be silently ignored — reject it explicitly), and every key name must be
// a legal environment-variable name. An omitted or empty Env is valid.
func (e ExecutorToolConfig) validate() error {
	for key := range e.Env {
		if key == "HOME" {
			return fmt.Errorf("tools.executor.bash.env: HOME is reserved (forced to the synthetic sandbox home)")
		}
		if !envKeyNameRe.MatchString(key) {
			return fmt.Errorf("tools.executor.bash.env: invalid key name %q (must match ^[A-Za-z_][A-Za-z0-9_]*)", key)
		}
	}
	return nil
}

// BoolPtr returns a pointer to b. It is the helper used for *bool config
// defaults such as ExecutorToolConfig.Network and LandlockConfig.Enabled, and is
// exported so callers (e.g. tests in other packages) can construct those fields.
func BoolPtr(b bool) *bool {
	return &b
}

// ToolsConfig groups all tool configuration.
type ToolsConfig struct {
	Xizhi    XizhiConfig    `yaml:"xizhi"`
	Webfetch WebfetchConfig `yaml:"webfetch"`
	Executor ExecutorConfig `yaml:"executor"`
	UserMCP  UserMCPConfig  `yaml:"user_mcp"`
	// Timeouts maps a built-in tool name (the same identifier agents reference in
	// their tools: lists) to a per-invocation execution budget. Enforced centrally
	// at the registry dispatch entry point (capability: tool-execution-timeout):
	// a tool mapped to a positive duration is bounded by that duration on every
	// Call; an absent entry or a zero/negative duration is unbounded (the prior
	// behavior). For remote tools that already have a native timeout (webfetch /
	// bash / per-user mcp), this acts as a looser outer backstop — the native
	// (tighter) bound still fires first in normal operation.
	Timeouts map[string]time.Duration `yaml:"timeouts"`
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

	// Shadow decode of the raw YAML for the checks the typed Config cannot
	// express (the fraction-truncation precedent): (1) model-effort-v2's
	// residual-field rejection — the removed keys (openai.model, top-level
	// openai.max_context_tokens, agents.<name>.model|thinking|reasoning_effort)
	// would otherwise be silently ignored by the typed decode, and a silently
	// dropped `thinking: true` in particular would turn reasoning off without
	// a trace; (2) every openai.models[].max_context_tokens MUST be an integer
	// — yaml.v3 silently truncates a fractional value into the int field
	// (12.5 → 12), so integer-ness is checked on the raw value (a float that
	// is not whole, or any non-numeric value, fails fast; context-compaction
	// spec: "a configured value MUST be a positive integer").
	var shadow struct {
		OpenAI map[string]any `yaml:"openai"`
	}
	if err := yaml.Unmarshal([]byte(expanded), &shadow); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if _, ok := shadow.OpenAI["model"]; ok {
		return nil, fmt.Errorf("config validation error: openai.model was removed (model-effort-v2): rename it to openai.title_model if it only served title generation; per-turn models now come from the openai.models catalog")
	}
	if _, ok := shadow.OpenAI["max_context_tokens"]; ok {
		return nil, fmt.Errorf("config validation error: openai.max_context_tokens was removed (model-effort-v2): set max_context_tokens on each openai.models catalog entry instead")
	}
	var residualAgentFields struct {
		Agents map[string]map[string]any `yaml:"agents"`
	}
	if err := yaml.Unmarshal([]byte(expanded), &residualAgentFields); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	for agentName, fields := range residualAgentFields.Agents {
		for _, removed := range []string{"model", "thinking", "reasoning_effort"} {
			if _, ok := fields[removed]; !ok {
				continue
			}
			switch removed {
			case "model":
				return nil, fmt.Errorf("config validation error: agents.%s.model was removed (model-effort-v2): the model now comes from the openai.models catalog (openai.default_model or the request `model` parameter)", agentName)
			case "thinking":
				return nil, fmt.Errorf("config validation error: agents.%s.thinking was removed (model-effort-v2): thinking is a catalog-entry capability (openai.models[].thinking) plus openai.default_reasoning_effort", agentName)
			case "reasoning_effort":
				return nil, fmt.Errorf("config validation error: agents.%s.reasoning_effort was removed (model-effort-v2): promote the level to openai.default_reasoning_effort if it was uniform across agents", agentName)
			}
		}
	}
	if rawModels, ok := shadow.OpenAI["models"].([]any); ok {
		for i, raw := range rawModels {
			entry, _ := raw.(map[string]any)
			if entry == nil {
				continue
			}
			switch v := entry["max_context_tokens"].(type) {
			case nil, int, uint, int64:
			case float64:
				if v != float64(int64(v)) {
					return nil, fmt.Errorf("config validation error: openai.models[%d].max_context_tokens: must be a positive integer (got %v)", i, v)
				}
			default:
				return nil, fmt.Errorf("config validation error: openai.models[%d].max_context_tokens: must be a positive integer (got %T)", i, v)
			}
		}
	}

	cfg.OpenAI.applyDefaults()
	cfg.Logging.applyDefaults()
	cfg.OnlyOffice.applyDefaults()
	cfg.Server.applyDefaults()
	cfg.Storage.Workspace.applyDefaults()
	cfg.Landlock.applyDefaults()
	cfg.Messages.applyDefaults()
	cfg.Tools.Executor.Bash.ApplyDefaults()
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
	if err := c.Messages.validate(); err != nil {
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
	if err := c.Tools.Executor.Bash.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.OpenAI.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	// output_schema × reasoning cross-check (model-effort-v2): structured
	// output and reasoning are mutually exclusive, and with a deployment-level
	// default effort a parameter-less turn would silently hit the conflict at
	// runtime — so fail the load when any output_schema agent coexists with a
	// non-none default. The runtime twin (request-resolved effort != none →
	// 400) lives in the handler.
	if c.OpenAI.DefaultReasoningEffort != "none" {
		for _, name := range []string{"confucius", "chongzhi", "liang"} {
			var cfg AgentConfig
			switch name {
			case "confucius":
				cfg = c.Agents.Confucius
			case "chongzhi":
				cfg = c.Agents.Chongzhi
			case "liang":
				cfg = c.Agents.Liang
			}
			if strings.TrimSpace(cfg.OutputSchema) != "" {
				return fmt.Errorf("config validation error: agents.%s.output_schema conflicts with openai.default_reasoning_effort %q (structured output and reasoning are mutually exclusive; use a none default and select effort per request)", name, c.OpenAI.DefaultReasoningEffort)
			}
		}
	}
	return nil
}

// ModelCatalog returns the deployment's model catalog (per-request-model-
// selection, made mandatory by model-effort-v2): the openai.models list
// verbatim. The catalog is required at load, so there is no implicit
// synthesized fallback anymore.
func (c *Config) ModelCatalog() []ModelCatalogEntry {
	return c.OpenAI.Models
}

// DefaultModelName returns the effective default model name: openai.default_model
// when set, else the first catalog entry. This is the model a parameter-less
// turn resolves to for run-meta/turn_usage recording and the compaction
// threshold.
func (c *Config) DefaultModelName() string {
	if c.OpenAI.DefaultModel != "" {
		return c.OpenAI.DefaultModel
	}
	return c.OpenAI.Models[0].Name
}

// TitleModelName resolves the title-generation model (model-effort-v2):
// openai.title_model when set, else the default catalog entry. Wiring calls
// this once and hands the resolved name to the TitleService.
func (c *Config) TitleModelName() string {
	if strings.TrimSpace(c.OpenAI.TitleModel) != "" {
		return c.OpenAI.TitleModel
	}
	return c.DefaultModelName()
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
