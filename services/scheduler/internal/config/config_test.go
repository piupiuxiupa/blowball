package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
