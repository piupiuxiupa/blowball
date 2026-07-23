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
    python:
      enabled: false
      timeout: 20s
      max_output_bytes: 4096
      allowed_env_patterns: ["PATH", "PYTHON*"]
      network: false
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
	if !bash.Network {
		t.Error("Tools.Executor.Bash.Network = false, want true")
	}

	python := cfg.Tools.Executor.Python
	if python.Enabled {
		t.Error("Tools.Executor.Python.Enabled = true, want false")
	}
	if python.Timeout != 20*time.Second {
		t.Errorf("Tools.Executor.Python.Timeout = %v, want 20s", python.Timeout)
	}
	if python.MaxOutputBytes != 4096 {
		t.Errorf("Tools.Executor.Python.MaxOutputBytes = %d, want 4096", python.MaxOutputBytes)
	}
	if python.Network {
		t.Error("Tools.Executor.Python.Network = true, want false")
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
	if bash.Network {
		t.Error("Tools.Executor.Bash.Network default = true, want false")
	}
	if len(bash.AllowedEnvPatterns) == 0 {
		t.Error("Tools.Executor.Bash.AllowedEnvPatterns should have defaults")
	}
}

func TestLoad_PipConfig(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    pip:
      enabled: true
      timeout: 90s
      max_output_bytes: 32768
      allowed_env_patterns: ["PATH", "PYTHON*"]
      network: false
      index_url: https://pypi.tuna.tsinghua.edu.cn/simple
      extra_index_urls:
        - https://extra.example.com/simple
      trusted_hosts:
        - pypi.tuna.tsinghua.edu.cn
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	pip := cfg.Tools.Executor.Pip
	if !pip.Enabled {
		t.Error("Tools.Executor.Pip.Enabled = false, want true")
	}
	if pip.Timeout != 90*time.Second {
		t.Errorf("Tools.Executor.Pip.Timeout = %v, want 90s", pip.Timeout)
	}
	if pip.MaxOutputBytes != 32768 {
		t.Errorf("Tools.Executor.Pip.MaxOutputBytes = %d, want 32768", pip.MaxOutputBytes)
	}
	if len(pip.AllowedEnvPatterns) != 2 || pip.AllowedEnvPatterns[0] != "PATH" {
		t.Errorf("unexpected pip env patterns: %v", pip.AllowedEnvPatterns)
	}
	if pip.NetworkEnabled() {
		t.Error("Tools.Executor.Pip.Network = true, want false")
	}
	if pip.IndexURL != "https://pypi.tuna.tsinghua.edu.cn/simple" {
		t.Errorf("Tools.Executor.Pip.IndexURL = %q, want %q", pip.IndexURL, "https://pypi.tuna.tsinghua.edu.cn/simple")
	}
	if len(pip.ExtraIndexURLs) != 1 || pip.ExtraIndexURLs[0] != "https://extra.example.com/simple" {
		t.Errorf("unexpected pip extra index URLs: %v", pip.ExtraIndexURLs)
	}
	if len(pip.TrustedHosts) != 1 || pip.TrustedHosts[0] != "pypi.tuna.tsinghua.edu.cn" {
		t.Errorf("unexpected pip trusted hosts: %v", pip.TrustedHosts)
	}
}

func TestLoad_PipConfigDefaults(t *testing.T) {
	path := writeTempYAML(t, `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "ok"
tools:
  executor:
    pip:
      enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	pip := cfg.Tools.Executor.Pip
	if pip.Timeout != 120*time.Second {
		t.Errorf("Tools.Executor.Pip.Timeout default = %v, want 120s", pip.Timeout)
	}
	if pip.MaxOutputBytes != 65536 {
		t.Errorf("Tools.Executor.Pip.MaxOutputBytes default = %d, want 65536", pip.MaxOutputBytes)
	}
	if !pip.NetworkEnabled() {
		t.Error("Tools.Executor.Pip.Network default = false, want true")
	}
	if len(pip.AllowedEnvPatterns) == 0 {
		t.Error("Tools.Executor.Pip.AllowedEnvPatterns should have defaults")
	}
}

func TestPipToolConfig_NetworkEnabledDefaultsToTrue(t *testing.T) {
	cfg := PipToolConfig{}
	if !cfg.NetworkEnabled() {
		t.Error("NetworkEnabled should default to true when Network is nil")
	}

	f := false
	cfg.Network = &f
	if cfg.NetworkEnabled() {
		t.Error("NetworkEnabled should return false when Network is explicitly false")
	}

	tr := true
	cfg.Network = &tr
	if !cfg.NetworkEnabled() {
		t.Error("NetworkEnabled should return true when Network is explicitly true")
	}
}

func TestPipToolConfig_ToExecutorToolConfig(t *testing.T) {
	f := false
	cfg := PipToolConfig{
		Enabled:            true,
		Timeout:            90 * time.Second,
		MaxOutputBytes:     32768,
		AllowedEnvPatterns: []string{"PATH"},
		Network:            &f,
	}

	exec := cfg.ToExecutorToolConfig()
	if !exec.Enabled {
		t.Error("Enabled mismatch")
	}
	if exec.Timeout != 90*time.Second {
		t.Errorf("Timeout mismatch: %v", exec.Timeout)
	}
	if exec.MaxOutputBytes != 32768 {
		t.Errorf("MaxOutputBytes mismatch: %d", exec.MaxOutputBytes)
	}
	if len(exec.AllowedEnvPatterns) != 1 || exec.AllowedEnvPatterns[0] != "PATH" {
		t.Errorf("AllowedEnvPatterns mismatch: %v", exec.AllowedEnvPatterns)
	}
	if exec.Network {
		t.Error("Network mismatch")
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
