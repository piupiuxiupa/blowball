package config

import (
	"os"
	"path/filepath"
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
  confuse:
    name: Confuse
    model: gpt-4o-mini
    system_prompt: "you are confuse"
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
	if cfg.Agents.Confuse.Name != "Confuse" {
		t.Errorf("Agents.Confuse.Name = %q", cfg.Agents.Confuse.Name)
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
  confuse: {name: Confuse}
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
  confuse:
    name: Confuse
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
  confuse:
    name: Confuse
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
			Confuse: AgentConfig{
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

	cfg.Agents.Confuse.MCP.Servers[0].Tools = []string{"web_search", "*"}
	if err := cfg.ValidateAgentMCPTools(serverTools); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfig_ValidateAgentSkills(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Confuse: AgentConfig{Skills: []string{"coding-style"}},
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

	cfg.Agents.Confuse.Skills = []string{"unknown"}
	if err := cfg.ValidateAgentSkills("", hasSkill); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
