package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const plaintextConfig = `
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
rpc:
  security_mode: plaintext_token
  listen: "127.0.0.1:9091"
  peer_tokens:
    blowball-main: main_config_test_token
main_service:
  api_endpoint: "127.0.0.1:9092"
  agent_endpoint: "127.0.0.1:9092"
  client_token: scheduler_config_test_token
scheduler:
  execution_enabled: false
`

func TestLoadRPCSecurityModes(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseConfig))
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModeMTLS, cfg.RPC.SecurityMode)
	cfg, err = Load(writeConfig(t, plaintextConfig))
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModePlaintextToken, cfg.RPC.SecurityMode)
	assert.Equal(t, "scheduler_config_test_token", cfg.MainService.ClientToken)
	assert.Equal(t, "main_config_test_token", cfg.RPC.PeerTokens[rpcx.IdentityMain])
	enabled, _, _, _, _, _, _, _, _ := cfg.Scheduler.Values()
	assert.False(t, enabled)
}

func TestLoadSchedulerConfigExample(t *testing.T) {
	t.Setenv("SCHEDULER_MYSQL_DSN", "test:test@tcp(127.0.0.1:3306)/example")
	cfg, err := Load("../../config.example.yaml")
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModeMTLS, cfg.RPC.SecurityMode)
	_, err = logger.InitForRole(cfg.Logging, t.TempDir(), "scheduler")
	require.NoError(t, err, "the shipped Scheduler example must initialize its logger")
}

func TestDefaultSchedulerLoggingInitializes(t *testing.T) {
	cfg, err := Load(writeConfig(t, plaintextConfig))
	require.NoError(t, err)
	_, err = logger.InitForRole(cfg.Logging, t.TempDir(), "scheduler")
	require.NoError(t, err)
}

func TestLoadPlaintextCredentialsValidation(t *testing.T) {
	t.Setenv("RPC_CONFIG_UNSET_TEST_TOKEN", "")
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty inbound", body: strings.Replace(plaintextConfig, "peer_tokens:\n    blowball-main: main_config_test_token", "peer_tokens: {}", 1), want: "rpc.peer_tokens"},
		{name: "empty outbound", body: strings.Replace(plaintextConfig, "scheduler_config_test_token", "", 1), want: "main_service.client_token"},
		{name: "missing environment", body: strings.Replace(plaintextConfig, "scheduler_config_test_token", "\"${RPC_CONFIG_UNSET_TEST_TOKEN}\"", 1), want: "main_service.client_token"},
		{name: "unknown identity", body: strings.Replace(plaintextConfig, "blowball-main:", "blowball-unknown:", 1), want: "peer_tokens"},
		{name: "ambiguous identity", body: strings.Replace(plaintextConfig, "    blowball-main: main_config_test_token", "    blowball-main: main_config_test_token\n    blowball-agent: main_config_test_token", 1), want: "peer_tokens"},
		{name: "whitespace", body: strings.Replace(plaintextConfig, "scheduler_config_test_token", "\"MAY_LEAK_SECRET invalid\"", 1), want: "client_token"},
		{name: "control", body: strings.Replace(plaintextConfig, "scheduler_config_test_token", "\"MAY_LEAK_SECRET\\u0000\"", 1), want: "client_token"},
		{name: "non ASCII", body: strings.Replace(plaintextConfig, "scheduler_config_test_token", "\"MAY_LEAK_SECRET_秘密\"", 1), want: "client_token"},
		{name: "unknown mode", body: strings.Replace(baseConfig, "rpc:", "rpc:\n  security_mode: auto", 1), want: "security_mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "MAY_LEAK")
		})
	}
}

func TestLoadRPCCredentialDecodeDoesNotLeak(t *testing.T) {
	body := strings.Replace(plaintextConfig,
		"peer_tokens:\n    blowball-main: main_config_test_token",
		"peer_tokens: MAY_LEAK_SECRET_wrong_shape", 1)
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "MAY_LEA", "even a truncated credential prefix must not be exposed")
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduler.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const baseConfig = `
mysql:
  dsn: "${TEST_SCHEDULER_DSN:user:pass@tcp(127.0.0.1:3306)/db}"
rpc:
  listen: ":9091"
  tls:
    cert_file: /certs/s.crt
    key_file: /certs/s.key
    ca_file: /certs/ca.crt
main_service:
  api_endpoint: "127.0.0.1:9092"
  agent_endpoint: "127.0.0.1:9092"
  tls:
    cert_file: /certs/c.crt
    key_file: /certs/c.key
    ca_file: /certs/ca.crt
scheduler:
  execution_enabled: true
`

func TestLoadAppliesDefaultsAndExpandsEnv(t *testing.T) {
	t.Setenv("TEST_SCHEDULER_DSN", "real:secret@tcp(db:3306)/blowball")
	cfg, err := Load(writeConfig(t, baseConfig))
	require.NoError(t, err)
	assert.Equal(t, "real:secret@tcp(db:3306)/blowball", cfg.MySQL.DSN)
	assert.Equal(t, ":9091", cfg.RPC.Listen)

	enabled, poll, maxRun, lease, backoff, concurrent, attempts, scan, maxInput := cfg.Scheduler.Values()
	assert.True(t, enabled)
	assert.Equal(t, 5*time.Second, poll)
	assert.Equal(t, 10*time.Minute, maxRun)
	assert.Equal(t, 30*time.Second, lease)
	assert.Equal(t, time.Second, backoff)
	assert.Equal(t, 4, concurrent)
	assert.Equal(t, 3, attempts)
	assert.Equal(t, 100, scan)
	assert.Equal(t, 5000, maxInput)
}

func TestLoadRejectsNonPositiveBounds(t *testing.T) {
	body := baseConfig + "  poll_interval: 0s\n"
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poll_interval must be positive")

	body = baseConfig + "  max_concurrent: -1\n"
	_, err = Load(writeConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_concurrent must be positive")
}

func TestLoadRequiresMTLSAndEndpoints(t *testing.T) {
	body := `
mysql:
  dsn: "user:pass@tcp(db)/db"
rpc:
  listen: ":9091"
main_service:
  api_endpoint: "127.0.0.1:9092"
  tls:
    cert_file: /c/c
    key_file: /c/k
    ca_file: /c/a
scheduler:
  execution_enabled: false
`
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rpc.tls")

	body = baseConfig + "  # no agent endpoint for execution\n"
	// Remove the agent endpoint from the base config.
	noAgent := `
mysql:
  dsn: "user:pass@tcp(db)/db"
rpc:
  listen: ":9091"
  tls:
    cert_file: /s.crt
    key_file: /s.key
    ca_file: /ca.crt
main_service:
  api_endpoint: "127.0.0.1:9092"
  tls:
    cert_file: /c.crt
    key_file: /c.key
    ca_file: /ca.crt
scheduler:
  execution_enabled: true
`
	_, err = Load(writeConfig(t, noAgent))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_endpoint")
}
