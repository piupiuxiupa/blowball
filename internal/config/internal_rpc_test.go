package config

import (
	"strings"
	"testing"
	"time"

	"github.com/lush/blowball/internal/rpcx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseInternalRPCYAML(extra string) string {
	return `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
internal_rpc:
  enabled: true
  listen: ":9092"
  tls: {cert_file: /c/main.crt, key_file: /c/main.key, ca_file: /c/ca.crt}
  scheduler_endpoint: "127.0.0.1:9091"
  scheduler_tls: {cert_file: /c/client.crt, key_file: /c/client.key, ca_file: /c/ca.crt, server_name: blowball-scheduler}
` + extra
}

const plaintextInternalRPCYAML = `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
internal_rpc:
  enabled: true
  security_mode: plaintext_token
  listen: "127.0.0.1:9092"
  scheduler_endpoint: "127.0.0.1:9091"
  peer_tokens:
    blowball-scheduler: scheduler_config_test_token
  client_token: main_config_test_token
`

func TestInternalRPCSecurityModes(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, baseInternalRPCYAML("")))
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModeMTLS, cfg.InternalRPC.SecurityMode)
	cfg, err = Load(writeTempYAML(t, plaintextInternalRPCYAML))
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModePlaintextToken, cfg.InternalRPC.SecurityMode)
	assert.Equal(t, "main_config_test_token", cfg.InternalRPC.ClientToken)
	assert.Equal(t, "scheduler_config_test_token", cfg.InternalRPC.PeerTokens[rpcx.IdentityScheduler])
	disabled := strings.Replace(plaintextInternalRPCYAML, "enabled: true", "enabled: false", 1)
	disabled = strings.Replace(disabled, "security_mode: plaintext_token", "security_mode: unsupported", 1)
	disabled = strings.Replace(disabled, "main_config_test_token", "", 1)
	cfg, err = Load(writeTempYAML(t, disabled))
	require.NoError(t, err)
	assert.False(t, cfg.InternalRPC.Enabled)
}

func TestInternalRPCConfigExample(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-example-config-test-only")
	t.Setenv("JWT_SECRET", "jwt-example-config-test-only")
	cfg, err := Load("../../config.example.yaml")
	require.NoError(t, err)
	assert.Equal(t, rpcx.SecurityModeMTLS, cfg.InternalRPC.SecurityMode)
}

func TestInternalRPCPlaintextValidation(t *testing.T) {
	t.Setenv("RPC_CONFIG_UNSET_TEST_TOKEN", "")
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty inbound", body: strings.Replace(plaintextInternalRPCYAML, "peer_tokens:\n    blowball-scheduler: scheduler_config_test_token", "peer_tokens: {}", 1), want: "peer_tokens"},
		{name: "empty outbound", body: strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "", 1), want: "client_token"},
		{name: "missing environment", body: strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "\"${RPC_CONFIG_UNSET_TEST_TOKEN}\"", 1), want: "client_token"},
		{name: "unknown identity", body: strings.Replace(plaintextInternalRPCYAML, "blowball-scheduler:", "blowball-unknown:", 1), want: "peer_tokens"},
		{name: "ambiguous binding", body: strings.Replace(plaintextInternalRPCYAML, "    blowball-scheduler: scheduler_config_test_token", "    blowball-scheduler: scheduler_config_test_token\n    blowball-main: scheduler_config_test_token", 1), want: "peer_tokens"},
		{name: "whitespace credential", body: strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "\"MAY_LEAK_SECRET invalid\"", 1), want: "client_token"},
		{name: "control character", body: strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "\"MAY_LEAK_SECRET\\u0000\"", 1), want: "client_token"},
		{name: "non ASCII", body: strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "\"MAY_LEAK_SECRET_秘密\"", 1), want: "client_token"},
		{name: "unknown mode", body: strings.Replace(baseInternalRPCYAML(""), "internal_rpc:", "internal_rpc:\n  security_mode: auto", 1), want: "security_mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeTempYAML(t, tt.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "MAY_LEAK")
		})
	}
}

func TestInternalRPCCredentialDecodeDoesNotLeak(t *testing.T) {
	body := strings.Replace(plaintextInternalRPCYAML,
		"peer_tokens:\n    blowball-scheduler: scheduler_config_test_token",
		"peer_tokens: MAY_LEAK_SECRET_wrong_shape", 1)
	_, err := Load(writeTempYAML(t, body))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "MAY_LEA", "even a truncated credential prefix must not be exposed")
}

func TestInternalRPCConfigValid(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, baseInternalRPCYAML(`
scheduled_runtime:
  poll_interval: 2s
  lease_duration: 10s
  max_concurrent: 3
  max_prepare_attempts: 2
  scan_batch: 20
scheduled_resources:
  max_files: 10
  max_total_bytes: 1024
  max_file_bytes: 512
  max_context_files: 5
  max_context_bytes: 100
`)))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InternalRPC.Enabled {
		t.Fatal("internal_rpc should be enabled")
	}
	poll, lease, concurrent, attempts, scan := cfg.ScheduledRuntime.Values()
	if poll != 2*time.Second || lease != 10*time.Second || concurrent != 3 || attempts != 2 || scan != 20 {
		t.Fatalf("unexpected runtime values: %s %s %d %d %d", poll, lease, concurrent, attempts, scan)
	}
	if cfg.ScheduledResources.MaxFiles != 10 || cfg.ScheduledResources.MaxContextBytes != 100 {
		t.Fatalf("unexpected resource values: %+v", cfg.ScheduledResources)
	}
}

func TestInternalRPCConfigRequiresTLSAndEndpoints(t *testing.T) {
	path := writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
internal_rpc:
  enabled: true
  listen: ":9092"
  scheduler_endpoint: "127.0.0.1:9091"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "internal_rpc.tls") {
		t.Fatalf("expected a mutual-TLS requirement error, got %v", err)
	}

	path = writeTempYAML(t, strings.Replace(baseInternalRPCYAML(""), `scheduler_endpoint: "127.0.0.1:9091"`, "", 1))
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "scheduler_endpoint") {
		t.Fatalf("expected a scheduler endpoint requirement error, got %v", err)
	}
}

func TestScheduledRuntimeAndResourcesRejectNegativeBounds(t *testing.T) {
	path := writeTempYAML(t, baseInternalRPCYAML(`
scheduled_runtime:
  max_concurrent: -1
`))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("expected a positive max_concurrent error, got %v", err)
	}

	path = writeTempYAML(t, baseInternalRPCYAML(`
scheduled_resources:
  max_files: -1
`))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_files") {
		t.Fatalf("expected a positive max_files error, got %v", err)
	}
}

func TestInternalRPCConfigDisabledIsZeroBehaviorChange(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalRPC.Enabled {
		t.Fatal("internal_rpc must be disabled by default")
	}
}
