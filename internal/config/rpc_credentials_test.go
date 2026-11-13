package config

import (
	"strings"
	"testing"

	"github.com/lush/blowball/internal/rpcx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInternalRPCTokenEnvironmentValues(t *testing.T) {
	t.Setenv("RPC_TOKEN_LITERAL_TEST", "must_not_expand")
	tests := []struct {
		name  string
		token string
	}{
		{name: "double quote", token: `MAY_LEAK_SECRET_"opaque`},
		{name: "single quote", token: "MAY_LEAK_SECRET_'opaque"},
		{name: "backslash", token: `MAY_LEAK_SECRET_\topaque`},
		{name: "two backslashes", token: `MAY_LEAK_SECRET_\\opaque`},
		{name: "literal reference", token: "MAY_LEAK_SECRET_${RPC_TOKEN_LITERAL_TEST}"},
		{name: "numeric", token: "1234567890"},
	}
	references := []struct {
		name  string
		value string
	}{
		{name: "double quoted", value: `"${RPC_TOKEN_ENV_TEST}"`},
		{name: "single quoted", value: "'${RPC_TOKEN_ENV_TEST}'"},
		{name: "plain", value: "${RPC_TOKEN_ENV_TEST}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, rpcx.ValidateClientToken(tt.token))
			t.Setenv("RPC_TOKEN_ENV_TEST", tt.token)
			for _, reference := range references {
				t.Run(reference.name, func(t *testing.T) {
					for _, field := range []string{"inbound", "outbound"} {
						t.Run(field, func(t *testing.T) {
							original := "scheduler_config_test_token"
							if field == "outbound" {
								original = "main_config_test_token"
							}
							body := strings.Replace(plaintextInternalRPCYAML, original, reference.value, 1)
							cfg, err := Load(writeTempYAML(t, body))
							require.NoError(t, err)
							got := cfg.InternalRPC.PeerTokens[rpcx.IdentityScheduler]
							if field == "outbound" {
								got = cfg.InternalRPC.ClientToken
							}
							assert.Equal(t, tt.token, got)
						})
					}
				})
			}
		})
	}
}

func TestInternalRPCTokenEnvironmentAliases(t *testing.T) {
	token := `MAY_LEAK_SECRET_"\opaque`
	t.Setenv("RPC_TOKEN_ENV_TEST", token)
	body := strings.Replace(plaintextInternalRPCYAML, "scheduler_config_test_token", `&rpc_token "${RPC_TOKEN_ENV_TEST}"`, 1)
	body = strings.Replace(body, "main_config_test_token", "*rpc_token", 1)
	for _, merged := range []bool{false, true} {
		name := "direct"
		if merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			configBody := body
			if merged {
				configBody = strings.Replace(body, "internal_rpc:", "rpc_defaults: &rpc_defaults", 1)
				configBody += "\ninternal_rpc:\n  <<: *rpc_defaults\n"
			}
			cfg, err := Load(writeTempYAML(t, configBody))
			require.NoError(t, err)
			assert.Equal(t, token, cfg.InternalRPC.ClientToken)
			assert.Equal(t, token, cfg.InternalRPC.PeerTokens[rpcx.IdentityScheduler])
		})
	}
}

func TestInternalRPCTokenInvalidEnvironmentDoesNotLeak(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{name: "space", token: "MAY_LEAK_SECRET invalid"},
		{name: "newline", token: "MAY_LEAK_SECRET\ninvalid"},
		{name: "control character", token: "MAY_LEAK_SECRET\x01invalid"},
		{name: "non ASCII", token: "MAY_LEAK_SECRET_秘密"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RPC_TOKEN_ENV_TEST", tt.token)
			for _, field := range []string{"inbound", "outbound"} {
				t.Run(field, func(t *testing.T) {
					original, want := "scheduler_config_test_token", "internal_rpc.peer_tokens"
					if field == "outbound" {
						original, want = "main_config_test_token", "internal_rpc.client_token"
					}
					body := strings.Replace(plaintextInternalRPCYAML, original, `"${RPC_TOKEN_ENV_TEST}"`, 1)
					_, err := Load(writeTempYAML(t, body))
					require.Error(t, err)
					assert.Contains(t, err.Error(), want)
					assert.NotContains(t, err.Error(), "MAY_LEAK")
				})
			}
		})
	}
}

func TestInternalRPCTokenEnvironmentPreservesLegacyScalarTypes(t *testing.T) {
	t.Setenv("RPC_TOKEN_ENV_TEST", `MAY_LEAK_SECRET_"\opaque`)
	t.Setenv("RPC_TOKEN_LEGACY_PORT", "8789")
	t.Setenv("RPC_TOKEN_LEGACY_ENABLED", "true")
	t.Setenv("RPC_TOKEN_LEGACY_CONTEXT", "10000")
	t.Setenv("RPC_TOKEN_LEGACY_LOG_FILE", "\n    max_size_mb: 17")
	body := strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", `"${RPC_TOKEN_ENV_TEST}"`, 1)
	body = strings.Replace(body, "enabled: true", "enabled: ${RPC_TOKEN_LEGACY_ENABLED}", 1)
	body = strings.Replace(body, "max_context_tokens: 10000", "max_context_tokens: ${RPC_TOKEN_LEGACY_CONTEXT}", 1)
	body += "\nserver:\n  port: ${RPC_TOKEN_LEGACY_PORT}\n"
	body += "\nlogging:\n  file: ${RPC_TOKEN_LEGACY_LOG_FILE}\n"
	cfg, err := Load(writeTempYAML(t, body))
	require.NoError(t, err)
	assert.True(t, cfg.InternalRPC.Enabled)
	assert.Equal(t, 8789, cfg.Server.Port)
	assert.Equal(t, 10000, cfg.OpenAI.Models[0].MaxContextTokens)
	assert.Equal(t, 17, cfg.Logging.File.MaxSizeMB)
}
