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
