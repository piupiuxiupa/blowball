package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempYAML writes content to a temp file and returns its path.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTempYAML(t, `
server:
  port: 9090
openai:
  api_key: sk-test
  base_url: https://api.openai.com/v1
  model: gpt-4o-mini
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
  host: 127.0.0.1
  port: 3306
  user: user
  password: pass
  dbname: db
redis:
  addr: 127.0.0.1:6379
  password: ""
  db: 0
jwt:
  secret: "super-secret"
  expire: 7d
agents:
  confucius:
    name: Confucius
    model: gpt-4o-mini
    system_prompt: "you are confucius"
    max_tokens: 2048
    tools: [chongzhi, liang]
  chongzhi:
    name: Chongzhi
    model: gpt-4o-mini
    system_prompt: "you are chongzhi"
    max_tokens: 4096
    tools: [read_file, write_file]
  liang:
    name: Liang
    model: gpt-4o-mini
    system_prompt: "you are liang"
    max_tokens: 2048
    tools: []
tools:
  xizhi:
    read: {enabled: true}
    write: {enabled: true}
    modify: {enabled: false}
logging:
  level: info
  format: json
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.JWT.Secret != "super-secret" {
		t.Errorf("JWT.Secret = %q, want %q", cfg.JWT.Secret, "super-secret")
	}
	if cfg.Agents.Confucius.Name != "Confucius" {
		t.Errorf("Agents.Confucius.Name = %q", cfg.Agents.Confucius.Name)
	}
	if len(cfg.Agents.Chongzhi.Tools) != 2 {
		t.Errorf("Agents.Chongzhi.Tools len = %d, want 2", len(cfg.Agents.Chongzhi.Tools))
	}
	if cfg.Tools.Xizhi.Modify.Enabled != false {
		t.Errorf("Tools.Xizhi.Modify.Enabled = true, want false")
	}

	d, err := cfg.JWT.ParseDuration()
	if err != nil {
		t.Fatalf("ParseDuration error: %v", err)
	}
	const day = 24 * time.Hour
	if d != 7*day {
		t.Errorf("JWT duration = %v, want %v", d, 7*day)
	}
}

func TestLoad_EnvSubstitution(t *testing.T) {
	t.Setenv("TEST_VAR", "from-env")
	t.Setenv("JWT_SECRET", "env-secret")
	t.Setenv("MYSQL_DSN", "env-user:env-pass@tcp(localhost:3306)/envdb")

	path := writeTempYAML(t, `
mysql:
  dsn: ${MYSQL_DSN}
jwt:
  secret: ${JWT_SECRET}
  expire: 1d
agents:
  confucius: {name: Confucius}
  chongzhi: {name: Chongzhi}
  liang: {name: Liang}
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.JWT.Secret != "env-secret" {
		t.Errorf("JWT.Secret = %q, want %q", cfg.JWT.Secret, "env-secret")
	}
	if cfg.MySQL.DSN != "env-user:env-pass@tcp(localhost:3306)/envdb" {
		t.Errorf("MySQL.DSN = %q, want env-substituted value", cfg.MySQL.DSN)
	}
}

func TestLoad_EnvSubstitution_WithDefault(t *testing.T) {
	// Ensure TEST_MISSING_VAR is genuinely unset.
	os.Unsetenv("TEST_MISSING_VAR")

	path := writeTempYAML(t, `
mysql:
  dsn: "${TEST_MISSING_VAR:fallback-dsn}"
jwt:
  secret: "any"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.MySQL.DSN != "fallback-dsn" {
		t.Errorf("MySQL.DSN = %q, want %q", cfg.MySQL.DSN, "fallback-dsn")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load expected error for missing file, got nil")
	}
}

func TestLoad_InvalidSecret(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: ""
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for empty jwt.secret, got nil")
	}
}

func TestLoad_InvalidDSN(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: ""
jwt:
  secret: "ok"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for empty mysql.dsn, got nil")
	}
}

func TestLoad_MCP_Valid(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: sse
      url: http://localhost:3001/sse
      headers:
        Authorization: Bearer token
      timeout: 10s
      call_timeout: 5s
      reconnect: true
      prefix: remote_
    - name: local
      transport: stdio
      command: ./mcp-server
      args: ["--stdio"]
      env:
        KEY: value
      timeout: 20s
      call_timeout: 15s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("MCP.Servers len = %d, want 2", len(cfg.MCP.Servers))
	}
	remote := cfg.MCP.Servers[0]
	if remote.Name != "remote" || remote.Transport != "sse" || remote.URL != "http://localhost:3001/sse" {
		t.Errorf("unexpected remote config: %+v", remote)
	}
	if remote.Timeout != 10*time.Second || remote.CallTimeout != 5*time.Second || !remote.Reconnect || remote.Prefix != "remote_" {
		t.Errorf("unexpected remote settings: timeout=%v call_timeout=%v reconnect=%v prefix=%q", remote.Timeout, remote.CallTimeout, remote.Reconnect, remote.Prefix)
	}
	if remote.Headers["Authorization"] != "Bearer token" {
		t.Errorf("remote Authorization header = %q, want %q", remote.Headers["Authorization"], "Bearer token")
	}
	local := cfg.MCP.Servers[1]
	if local.Name != "local" || local.Transport != "stdio" || local.Command != "./mcp-server" {
		t.Errorf("unexpected local config: %+v", local)
	}
	if len(local.Args) != 1 || local.Args[0] != "--stdio" || local.Env["KEY"] != "value" {
		t.Errorf("unexpected local args/env: args=%v env=%v", local.Args, local.Env)
	}
}

func TestLoad_MCP_EnvSubstitution(t *testing.T) {
	t.Setenv("MCP_TOKEN", "secret-token")
	t.Setenv("MCP_CMD", "./env-mcp")

	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: sse_server
      transport: sse
      url: http://localhost:3001/sse
      headers:
        Authorization: Bearer ${MCP_TOKEN}
    - name: stdio_server
      transport: stdio
      command: ${MCP_CMD}
      env:
        API_KEY: ${MCP_TOKEN}
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("MCP.Servers len = %d, want 2", len(cfg.MCP.Servers))
	}
	if cfg.MCP.Servers[0].Headers["Authorization"] != "Bearer secret-token" {
		t.Errorf("sse header = %q, want %q", cfg.MCP.Servers[0].Headers["Authorization"], "Bearer secret-token")
	}
	if cfg.MCP.Servers[1].Command != "./env-mcp" {
		t.Errorf("stdio command = %q, want %q", cfg.MCP.Servers[1].Command, "./env-mcp")
	}
	if cfg.MCP.Servers[1].Env["API_KEY"] != "secret-token" {
		t.Errorf("stdio env API_KEY = %q, want %q", cfg.MCP.Servers[1].Env["API_KEY"], "secret-token")
	}
}

func TestLoad_MCP_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name: "missing name",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - transport: sse
      url: http://localhost:3001/sse
`,
		},
		{
			name: "missing transport",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      url: http://localhost:3001/sse
`,
		},
		{
			name: "sse missing url",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: sse
`,
		},
		{
			name: "http missing url",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: http
`,
		},
		{
			name: "stdio missing command",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: local
      transport: stdio
`,
		},
		{
			name: "unsupported transport",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: websocket
`,
		},
		{
			name: "duplicate name",
			content: `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: sse
      url: http://localhost:3001/sse
    - name: remote
      transport: stdio
      command: ./mcp
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load expected validation error for %q, got nil", tc.name)
			}
		})
	}
}

func TestLoad_AgentMCP_UnknownServer(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
mcp:
  servers:
    - name: remote
      transport: sse
      url: http://localhost:3001/sse
agents:
  confucius:
    name: Confucius
    mcp:
      servers:
        - name: missing
          tools: ["*"]
  chongzhi: {name: Chongzhi}
  liang: {name: Liang}
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for unknown agent MCP server")
	}
}

func TestLoad_AgentMCP_EmptyServerName(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
agents:
  confucius:
    name: Confucius
    mcp:
      servers:
        - name: ""
          tools: ["*"]
  chongzhi: {name: Chongzhi}
  liang: {name: Liang}
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for empty agent MCP server name")
	}
}

func TestConfig_ValidateAgentMCPTools(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Confucius: AgentConfig{
				MCP: AgentMCPConfig{
					Servers: []AgentMCPServerConfig{
						{Name: "remote", Tools: []string{"web_search", "missing"}},
					},
				},
			},
		},
	}
	serverTools := map[string]map[string]struct{}{
		"remote": {"web_search": {}, "fetch_url": {}},
	}
	if err := cfg.ValidateAgentMCPTools(serverTools); err == nil {
		t.Fatal("expected error for unknown tool")
	}

	cfg.Agents.Confucius.MCP.Servers[0].Tools = []string{"web_search", "*"}
	if err := cfg.ValidateAgentMCPTools(serverTools); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_ReasoningConfig(t *testing.T) {
	cases := []struct {
		name        string
		agentBlock  string
		wantEffort  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "thinking true defaults to medium",
			agentBlock: "confucius:\n    name: Confucius\n    thinking: true",
			wantEffort: "medium",
		},
		{
			name:       "thinking true with low effort",
			agentBlock: "confucius:\n    name: Confucius\n    thinking: true\n    reasoning_effort: low",
			wantEffort: "low",
		},
		{
			name:       "thinking true with high effort",
			agentBlock: "confucius:\n    name: Confucius\n    thinking: true\n    reasoning_effort: high",
			wantEffort: "high",
		},
		{
			name:        "thinking true with invalid effort fails",
			agentBlock:  "confucius:\n    name: Confucius\n    thinking: true\n    reasoning_effort: ultra",
			wantErr:     true,
			errContains: "reasoning_effort",
		},
		{
			name:        "reasoning_effort set without thinking fails",
			agentBlock:  "confucius:\n    name: Confucius\n    reasoning_effort: low",
			wantErr:     true,
			errContains: "reasoning_effort",
		},
		{
			name:       "thinking false ignores absent reasoning_effort",
			agentBlock: "confucius:\n    name: Confucius\n    thinking: false",
			wantEffort: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, fmt.Sprintf(`
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
agents:
  %s
  chongzhi: {name: Chongzhi}
  liang: {name: Liang}
`, tc.agentBlock))

			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load expected error for %q, got nil", tc.name)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if cfg.Agents.Confucius.ReasoningEffort != tc.wantEffort {
				t.Errorf("Agents.Confucius.ReasoningEffort = %q, want %q", cfg.Agents.Confucius.ReasoningEffort, tc.wantEffort)
			}
		})
	}
}

func TestLoad_OutputSchemaConfig(t *testing.T) {
	cases := []struct {
		name        string
		agentBlock  string
		wantErr     bool
		errContains string
	}{
		{
			name:       "output_schema without thinking is valid",
			agentBlock: "liang:\n    name: Liang\n    output_schema: '{\"type\":\"object\"}'",
			wantErr:    false,
		},
		{
			name:        "output_schema with thinking rejected",
			agentBlock:  "liang:\n    name: Liang\n    thinking: true\n    output_schema: '{\"type\":\"object\"}'",
			wantErr:     true,
			errContains: "output_schema",
		},
		{
			name:        "invalid JSON output_schema rejected",
			agentBlock:  "liang:\n    name: Liang\n    output_schema: 'not-json'",
			wantErr:     true,
			errContains: "output_schema",
		},
		{
			name:        "retry negative max_attempts rejected",
			agentBlock:  "liang:\n    name: Liang\n    retry:\n        enabled: true\n        max_attempts: -1",
			wantErr:     true,
			errContains: "max_attempts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, fmt.Sprintf(`
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
agents:
  confucius: {name: Confucius}
  chongzhi: {name: Chongzhi}
  %s
`, tc.agentBlock))

			cfg, err := Load(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load expected error for %q, got nil", tc.name)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			_ = cfg
		})
	}
}

// TestLoad_RetryDefaults verifies the per-agent retry defaults: Liang defaults
// to retry-enabled with the standard backoff, Chongzhi defaults to disabled,
// and an explicit retry block is respected.
func TestLoad_RetryDefaults(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
agents:
  confucius: {name: Confucius}
  chongzhi: {name: Chongzhi}
  liang: {name: Liang}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// Liang (read-only) defaults to retry-enabled with standard backoffs.
	if !cfg.Agents.Liang.Retry.Enabled {
		t.Error("Liang retry should default to enabled")
	}
	if cfg.Agents.Liang.Retry.MaxAttempts != defaultRetryMaxAttempts {
		t.Errorf("Liang MaxAttempts = %d, want %d", cfg.Agents.Liang.Retry.MaxAttempts, defaultRetryMaxAttempts)
	}
	if cfg.Agents.Liang.Retry.InitialBackoff != defaultRetryInitialBackoff {
		t.Errorf("Liang InitialBackoff = %v, want %v", cfg.Agents.Liang.Retry.InitialBackoff, defaultRetryInitialBackoff)
	}
	if cfg.Agents.Liang.Retry.MaxBackoff != defaultRetryMaxBackoff {
		t.Errorf("Liang MaxBackoff = %v, want %v", cfg.Agents.Liang.Retry.MaxBackoff, defaultRetryMaxBackoff)
	}

	// Chongzhi (side-effecting) defaults to retry-disabled.
	if cfg.Agents.Chongzhi.Retry.Enabled {
		t.Error("Chongzhi retry should default to disabled")
	}
}

func TestLoad_ExecutorConfig(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    bash:
      enabled: true
      timeout: 45s
      max_output_bytes: 8192
      allowed_env_patterns: ["PATH", "HOME"]
      network: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	bash := cfg.Tools.Executor.Bash
	if !bash.Enabled {
		t.Error("Tools.Executor.Bash.Enabled = false, want true")
	}
	if bash.Timeout != 45*time.Second {
		t.Errorf("Tools.Executor.Bash.Timeout = %v, want 45s", bash.Timeout)
	}
	if bash.MaxOutputBytes != 8192 {
		t.Errorf("Tools.Executor.Bash.MaxOutputBytes = %d, want 8192", bash.MaxOutputBytes)
	}
	if len(bash.AllowedEnvPatterns) != 2 || bash.AllowedEnvPatterns[0] != "PATH" {
		t.Errorf("unexpected bash env patterns: %v", bash.AllowedEnvPatterns)
	}
	if !bash.NetworkEnabled() {
		t.Error("Tools.Executor.Bash.Network = false, want true")
	}
}

// TestLoad_ExecutorEnv pins the executor-tools spec's operator env-literal
// requirement: valid env round-trips, an empty map is a zero-behavior-change,
// and values are subject to the global ${VAR} / ${VAR:default} expansion applied
// to the whole YAML document at load (secrets may reference the host env).
func TestLoad_ExecutorEnv(t *testing.T) {
	t.Run("valid env passes and round-trips", func(t *testing.T) {
		path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    bash:
      enabled: true
      env:
        PIP_INDEX_URL: "https://pypi.example.com/simple"
        NODE_USE_ENV_PROXY: "1"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		env := cfg.Tools.Executor.Bash.Env
		if got, want := env["PIP_INDEX_URL"], "https://pypi.example.com/simple"; got != want {
			t.Errorf("env[PIP_INDEX_URL] = %q, want %q", got, want)
		}
		if got, want := env["NODE_USE_ENV_PROXY"], "1"; got != want {
			t.Errorf("env[NODE_USE_ENV_PROXY] = %q, want %q", got, want)
		}
	})

	t.Run("empty map is zero behavior change", func(t *testing.T) {
		path := writeTempYAML(t, minimalValidYAML+`
tools:
  executor:
    bash:
      env: {}
`)
		if _, err := Load(path); err != nil {
			t.Fatalf("Load with empty env map returned error: %v", err)
		}
	})

	t.Run("${VAR} value expands from host environment", func(t *testing.T) {
		t.Setenv("BLOWBALL_TEST_HOST_VAR", "from-host")
		path := writeTempYAML(t, minimalValidYAML+`
tools:
  executor:
    bash:
      env:
        KEY: "${BLOWBALL_TEST_HOST_VAR}"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if got, want := cfg.Tools.Executor.Bash.Env["KEY"], "from-host"; got != want {
			t.Errorf("env[KEY] = %q, want %q (host var expansion)", got, want)
		}
	})

	t.Run("${VAR:default} value expands with default when unset", func(t *testing.T) {
		os.Unsetenv("BLOWBALL_TEST_UNSET_VAR")
		path := writeTempYAML(t, minimalValidYAML+`
tools:
  executor:
    bash:
      env:
        LOG_LEVEL: "${BLOWBALL_TEST_UNSET_VAR:info}"
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if got, want := cfg.Tools.Executor.Bash.Env["LOG_LEVEL"], "info"; got != want {
			t.Errorf("env[LOG_LEVEL] = %q, want %q (default expansion)", got, want)
		}
	})
}

// TestLoad_ExecutorEnvRejections pins the fail-fast env guards: HOME is a
// reserved key and an invalid key name (space, empty, or digit-led) is rejected
// at config load, in the ParseMounts style.
func TestLoad_ExecutorEnvRejections(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{"HOME reserved", "HOME: /custom\n", "HOME is reserved"},
		{"key with space", `"bad key": v` + "\n", "invalid key name"},
		{"empty key", `"": v` + "\n", "invalid key name"},
		{"digit-led key", `"1ABC": v` + "\n", "invalid key name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := "tools:\n  executor:\n    bash:\n      env:\n        " + tc.entry
			_, err := Load(writeTempYAML(t, minimalValidYAML+block))
			if err == nil {
				t.Fatalf("Load expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoad_ExecutorConfigDefaults(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    bash:
      enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	bash := cfg.Tools.Executor.Bash
	if bash.Timeout != 30*time.Second {
		t.Errorf("Tools.Executor.Bash.Timeout default = %v, want 30s", bash.Timeout)
	}
	if bash.MaxOutputBytes != 65536 {
		t.Errorf("Tools.Executor.Bash.MaxOutputBytes default = %d, want 65536", bash.MaxOutputBytes)
	}
	if !bash.NetworkEnabled() {
		t.Error("Tools.Executor.Bash.Network default = false, want true")
	}
	if len(bash.AllowedEnvPatterns) == 0 {
		t.Error("Tools.Executor.Bash.AllowedEnvPatterns should have defaults")
	}
}

// TestLoad_ExecutorConfig_PythonPipBlocksIgnored pins the executor-tools spec
// scenario "python/pip config blocks are ignored": residual
// tools.executor.python / tools.executor.pip blocks (left over after upgrading
// a config that pre-dates the executor slim-down) are parsed by non-strict YAML
// unmarshal and silently ignored — no tools are registered from them and startup
// succeeds. Python/pip now run via bash.
func TestLoad_ExecutorConfig_PythonPipBlocksIgnored(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    bash:
      enabled: true
    python:
      enabled: true
    pip:
      enabled: true
      index_url: https://pypi.tuna.tsinghua.edu.cn/simple
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should succeed with residual python/pip blocks: %v", err)
	}
	if !cfg.Tools.Executor.Bash.Enabled {
		t.Error("bash should remain enabled")
	}
}

func TestConfig_ValidateAgentSkills(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Confucius: AgentConfig{Skills: []string{"coding-style"}},
		},
	}
	hasSkill := func(name, userID string) bool {
		if name == "coding-style" {
			return true
		}
		return false
	}
	if err := cfg.ValidateAgentSkills("", hasSkill); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg.Agents.Confucius.Skills = []string{"unknown"}
	if err := cfg.ValidateAgentSkills("", hasSkill); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

func TestLoad_LoggingDefaults(t *testing.T) {
	// No logging block at all: defaults must populate.
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format default = %q, want %q", cfg.Logging.Format, "json")
	}
	if len(cfg.Logging.Output) != 2 || cfg.Logging.Output[0] != "stderr" || cfg.Logging.Output[1] != "file" {
		t.Errorf("Logging.Output default = %v, want [stderr file]", cfg.Logging.Output)
	}
	def := DefaultLogFileConfig()
	if cfg.Logging.File.MaxSizeMB != def.MaxSizeMB {
		t.Errorf("Logging.File.MaxSizeMB default = %d, want %d", cfg.Logging.File.MaxSizeMB, def.MaxSizeMB)
	}
	if cfg.Logging.File.MaxBackups != def.MaxBackups {
		t.Errorf("Logging.File.MaxBackups default = %d, want %d", cfg.Logging.File.MaxBackups, def.MaxBackups)
	}
	if cfg.Logging.File.MaxAgeDays != def.MaxAgeDays {
		t.Errorf("Logging.File.MaxAgeDays default = %d, want %d", cfg.Logging.File.MaxAgeDays, def.MaxAgeDays)
	}
}

func TestLoad_LoggingInvalidFormat(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
logging:
  format: xml
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for invalid logging.format, got nil")
	}
	if !strings.Contains(err.Error(), "logging.format") {
		t.Errorf("error %q does not mention logging.format", err.Error())
	}
}

func TestLoad_LoggingInvalidOutput(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
logging:
  output: ["syslog"]
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load expected validation error for invalid logging.output sink, got nil")
	}
	if !strings.Contains(err.Error(), "logging.output") {
		t.Errorf("error %q does not mention logging.output", err.Error())
	}
}

func TestLoad_LoggingExplicitValues(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
logging:
  level: debug
  format: console
  output: ["stdout"]
  file:
    max_size_mb: 5
    max_backups: 2
    max_age_days: 7
    compress: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Logging.Format != "console" {
		t.Errorf("Logging.Format = %q, want console", cfg.Logging.Format)
	}
	if len(cfg.Logging.Output) != 1 || cfg.Logging.Output[0] != "stdout" {
		t.Errorf("Logging.Output = %v, want [stdout]", cfg.Logging.Output)
	}
	if cfg.Logging.File.MaxSizeMB != 5 {
		t.Errorf("Logging.File.MaxSizeMB = %d, want 5", cfg.Logging.File.MaxSizeMB)
	}
	if !cfg.Logging.File.Compress {
		t.Error("Logging.File.Compress = false, want true")
	}
}

func TestAuthIsPasswordRequired_DefaultsToTrue(t *testing.T) {
	// Omitting auth.password_required must preserve the password-based default.
	var cfg AuthConfig
	if !cfg.IsPasswordRequired() {
		t.Error("IsPasswordRequired() = false for unset value, want true (default)")
	}

	off := false
	cfg = AuthConfig{PasswordRequired: &off}
	if cfg.IsPasswordRequired() {
		t.Error("IsPasswordRequired() = true for explicit false, want false")
	}

	on := true
	cfg = AuthConfig{PasswordRequired: &on}
	if !cfg.IsPasswordRequired() {
		t.Error("IsPasswordRequired() = false for explicit true, want true")
	}
}

func TestLoad_AuthPasswordRequired(t *testing.T) {
	t.Run("explicit false loads", func(t *testing.T) {
		path := writeTempYAML(t, `
server:
  port: 9090
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
redis:
  addr: 127.0.0.1:6379
auth:
  password_required: false
jwt:
  secret: "super-secret"
  expire: 7d
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if cfg.Auth.IsPasswordRequired() {
			t.Fatal("IsPasswordRequired() = true, want false")
		}
	})

	t.Run("omitted defaults to true", func(t *testing.T) {
		path := writeTempYAML(t, `
server:
  port: 9090
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
redis:
  addr: 127.0.0.1:6379
jwt:
  secret: "super-secret"
  expire: 7d
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if !cfg.Auth.IsPasswordRequired() {
			t.Fatal("IsPasswordRequired() = false, want true (default)")
		}
	})
}

func TestLoad_StorageBackendDefault(t *testing.T) {
	// Omitting storage entirely must default to "local" (zero-behavior-change).
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Storage.Workspace.Backend != WorkspaceBackendLocal {
		t.Errorf("Storage.Workspace.Backend = %q, want %q", cfg.Storage.Workspace.Backend, WorkspaceBackendLocal)
	}
	if cfg.Storage.Workspace.IsShared() {
		t.Error("IsShared() = true, want false for default local backend")
	}
}

func TestLoad_StorageBackendShared(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
storage:
  workspace:
    backend: shared
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Storage.Workspace.Backend != WorkspaceBackendShared {
		t.Errorf("Storage.Workspace.Backend = %q, want %q", cfg.Storage.Workspace.Backend, WorkspaceBackendShared)
	}
	if !cfg.Storage.Workspace.IsShared() {
		t.Error("IsShared() = false, want true")
	}
}

func TestLoad_StorageBackendEnvSubstitution(t *testing.T) {
	// storage.workspace.backend must honor ${VAR} expansion like other fields.
	t.Setenv("WS_BACKEND", "shared")
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
storage:
  workspace:
    backend: ${WS_BACKEND}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Storage.Workspace.IsShared() {
		t.Errorf("Storage.Workspace.Backend = %q, want shared after expansion", cfg.Storage.Workspace.Backend)
	}
}

func TestLoad_StorageBackendInvalid(t *testing.T) {
	// An unrecognized backend fails fast at load time. "" is NOT in this list:
	// an empty value is valid because it defaults to "local".
	for _, b := range []string{"nfs", "s3", "object-store"} {
		path := writeTempYAML(t, fmt.Sprintf(`
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
storage:
  workspace:
    backend: %q
`, b))
		_, err := Load(path)
		if err == nil {
			t.Fatalf("Load expected validation error for backend %q, got nil", b)
		}
		if !strings.Contains(err.Error(), "storage.workspace.backend") {
			t.Errorf("error %q does not mention storage.workspace.backend", err.Error())
		}
	}
}

func TestWorkspaceStorageConfig_NormalizesCase(t *testing.T) {
	// Backend is lower-cased on load so "Shared" / "LOCAL" are accepted.
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
storage:
  workspace:
    backend: SHARED
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Storage.Workspace.IsShared() {
		t.Errorf("Storage.Workspace.Backend = %q, want shared after normalization", cfg.Storage.Workspace.Backend)
	}
}

// minimalValidYAML is the smallest config that passes validation; sandbox /
// landlock-focused tests append their own block to it.
const minimalValidYAML = `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
`

func TestLoad_LandlockDefaults(t *testing.T) {
	// Omitting the landlock block reproduces the pre-configurability baseline.
	cfg, err := Load(writeTempYAML(t, minimalValidYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.Landlock.IsEnabled() {
		t.Error("Landlock.IsEnabled() = false, want true (default)")
	}
	if got, want := cfg.Landlock.SystemReadOnly, DefaultLandlockSystemReadOnly(); !sliceEq(got, want) {
		t.Errorf("Landlock.SystemReadOnly = %v, want %v", got, want)
	}
	// The landlock baseline includes /proc (process-scope restriction needs it).
	if !containsStr(cfg.Landlock.SystemReadOnly, "/proc") {
		t.Errorf("Landlock.SystemReadOnly = %v, want it to contain /proc", cfg.Landlock.SystemReadOnly)
	}
	if len(cfg.Landlock.ExtraReadWrite) != 0 || len(cfg.Landlock.ExtraReadOnly) != 0 {
		t.Errorf("Landlock extra_* should default to empty, got rw=%v ro=%v", cfg.Landlock.ExtraReadWrite, cfg.Landlock.ExtraReadOnly)
	}
}

func TestLoad_LandlockExplicit(t *testing.T) {
	t.Run("enabled false disables", func(t *testing.T) {
		cfg, err := Load(writeTempYAML(t, minimalValidYAML+`
landlock:
  enabled: false
`))
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if cfg.Landlock.IsEnabled() {
			t.Fatal("Landlock.IsEnabled() = true, want false")
		}
	})

	t.Run("extra dirs load", func(t *testing.T) {
		cfg, err := Load(writeTempYAML(t, minimalValidYAML+`
landlock:
  extra_read_write: ["/var/cache/blowball"]
  extra_read_only: ["/opt/data"]
`))
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		if got, want := cfg.Landlock.ExtraReadWrite, []string{"/var/cache/blowball"}; !sliceEq(got, want) {
			t.Errorf("ExtraReadWrite = %v, want %v", got, want)
		}
		if got, want := cfg.Landlock.ExtraReadOnly, []string{"/opt/data"}; !sliceEq(got, want) {
			t.Errorf("ExtraReadOnly = %v, want %v", got, want)
		}
	})
}

func TestLoad_LandlockValidation(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{"relative extra_read_write", "landlock:\n  extra_read_write: [\"data/models\"]\n", "absolute"},
		{"relative extra_read_only", "landlock:\n  extra_read_only: [\"rel\"]\n", "absolute"},
		{"relative system_read_only", "landlock:\n  system_read_only: [\"rel\"]\n", "absolute"},
		{"broad extra_read_write", "landlock:\n  extra_read_write: [\"/\"]\n", "too broad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempYAML(t, minimalValidYAML+tc.block))
			if err == nil {
				t.Fatalf("Load expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoad_SandboxDefaults(t *testing.T) {
	// Omitting the sandbox block reproduces the pre-configurability baseline.
	cfg, err := Load(writeTempYAML(t, minimalValidYAML))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got, want := cfg.Tools.Executor.Sandbox.SystemReadOnly, DefaultExecutorSystemReadOnly(); !sliceEq(got, want) {
		t.Errorf("Sandbox.SystemReadOnly = %v, want %v", got, want)
	}
	// The executor baseline does NOT include /proc (bwrap synthesizes it).
	if containsStr(cfg.Tools.Executor.Sandbox.SystemReadOnly, "/proc") {
		t.Errorf("Sandbox.SystemReadOnly = %v, must not contain /proc", cfg.Tools.Executor.Sandbox.SystemReadOnly)
	}
}

func TestLoad_SandboxExtraMountsParsed(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, minimalValidYAML+`
tools:
  executor:
    sandbox:
      extra_read_only:
        - "/opt/models"
        - "/srv/datasets:/data"
      extra_read_write:
        - "/srv/cache"
`))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	ro := cfg.Tools.Executor.Sandbox.ExtraReadOnlyMounts
	if len(ro) != 2 || ro[0] != (MountSpec{Host: "/opt/models", Target: "/opt/models"}) || ro[1] != (MountSpec{Host: "/srv/datasets", Target: "/data"}) {
		t.Errorf("ExtraReadOnlyMounts = %v, want [{/opt/models /opt/models} {/srv/datasets /data}]", ro)
	}
	rw := cfg.Tools.Executor.Sandbox.ExtraReadWriteMounts
	if len(rw) != 1 || rw[0] != (MountSpec{Host: "/srv/cache", Target: "/srv/cache"}) {
		t.Errorf("ExtraReadWriteMounts = %v, want [{/srv/cache /srv/cache}]", rw)
	}
}

func TestLoad_SandboxValidation(t *testing.T) {
	cases := []struct {
		name    string
		block   string
		wantErr string
	}{
		{"broad extra_read_write", "tools:\n  executor:\n    sandbox:\n      extra_read_write: [\"/\"]\n", "too broad"},
		{"relative host", "tools:\n  executor:\n    sandbox:\n      extra_read_only: [\"data/models\"]\n", "absolute"},
		{"empty host:target target", "tools:\n  executor:\n    sandbox:\n      extra_read_only: [\"/opt/models:\"]\n", "empty"},
		{"target conflicts /workspace", "tools:\n  executor:\n    sandbox:\n      extra_read_only: [\"/x:/workspace\"]\n", "conflict"},
		{"target conflicts /home", "tools:\n  executor:\n    sandbox:\n      extra_read_write: [\"/x:/home\"]\n", "conflict"},
		{"target conflicts system baseline", "tools:\n  executor:\n    sandbox:\n      system_read_only: [\"/usr\"]\n      extra_read_only: [\"/x:/usr\"]\n", "conflict"},
		{"relative system_read_only", "tools:\n  executor:\n    sandbox:\n      system_read_only: [\"rel\"]\n", "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempYAML(t, minimalValidYAML+tc.block))
			if err == nil {
				t.Fatalf("Load expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseMounts(t *testing.T) {
	t.Run("host only defaults target to host", func(t *testing.T) {
		got, err := ParseMounts([]string{"/opt/models"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != (MountSpec{Host: "/opt/models", Target: "/opt/models"}) {
			t.Errorf("ParseMounts = %v, want [{/opt/models /opt/models}]", got)
		}
	})
	t.Run("host:target custom target", func(t *testing.T) {
		got, err := ParseMounts([]string{"/srv/datasets:/data"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != (MountSpec{Host: "/srv/datasets", Target: "/data"}) {
			t.Errorf("ParseMounts = %v, want [{/srv/datasets /data}]", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		got, err := ParseMounts(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ParseMounts(nil) = %v, want empty", got)
		}
	})
	t.Run("relative host rejected", func(t *testing.T) {
		if _, err := ParseMounts([]string{"data/models"}); err == nil {
			t.Fatal("expected error for relative host, got nil")
		}
	})
	t.Run("empty entry rejected", func(t *testing.T) {
		if _, err := ParseMounts([]string{""}); err == nil {
			t.Fatal("expected error for empty entry, got nil")
		}
	})
	t.Run("empty target rejected", func(t *testing.T) {
		if _, err := ParseMounts([]string{"/opt/models:"}); err == nil {
			t.Fatal("expected error for empty target, got nil")
		}
	})
}

func TestValidateLandlockRW(t *testing.T) {
	if err := ValidateLandlockRW(true, nil, nil); err == nil {
		t.Error("expected error for enabled landlock with no RW dirs")
	}
	if err := ValidateLandlockRW(true, []string{"/d/data", "/d/logs", "/d/skills"}, nil); err != nil {
		t.Errorf("unexpected error with default RW dirs: %v", err)
	}
	if err := ValidateLandlockRW(true, nil, []string{"/extra"}); err != nil {
		t.Errorf("unexpected error with extra RW dir: %v", err)
	}
	// Disabled landlock never errors regardless of the RW set.
	if err := ValidateLandlockRW(false, nil, nil); err != nil {
		t.Errorf("unexpected error for disabled landlock: %v", err)
	}
}

func TestLandlockIsEnabled_DefaultsToTrue(t *testing.T) {
	var l LandlockConfig
	if !l.IsEnabled() {
		t.Error("IsEnabled() = false for unset value, want true (default)")
	}
	off := false
	l = LandlockConfig{Enabled: &off}
	if l.IsEnabled() {
		t.Error("IsEnabled() = true for explicit false, want false")
	}
	on := true
	l = LandlockConfig{Enabled: &on}
	if !l.IsEnabled() {
		t.Error("IsEnabled() = false for explicit true, want true")
	}
}

// sliceEq reports whether two string slices are element-wise equal.
func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsStr reports whether s contains v.
func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
