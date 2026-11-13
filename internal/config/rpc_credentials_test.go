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

func TestInternalRPCTokenEnvironmentSequenceBlockAliases(t *testing.T) {
	token := `test_"\credential`
	t.Setenv("RPC_TOKEN_SEQUENCE_ALIAS", token)
	layouts := []struct {
		name         string
		anchorPrefix string
		tokenIndent  string
	}{
		{
			name:         "indentless sequence",
			anchorPrefix: "rpc_token_defaults:\n- token: &rpc_token ",
			tokenIndent:  "    ",
		},
		{
			name:         "indented sequence",
			anchorPrefix: "rpc_token_defaults:\n  - token: &rpc_token ",
			tokenIndent:  "      ",
		},
		{
			name:         "nested sequence",
			anchorPrefix: "rpc_token_defaults:\n  roles:\n    - token: &rpc_token ",
			tokenIndent:  "        ",
		},
	}
	styles := []struct {
		name  string
		value string
	}{
		{name: "literal", value: "|-"},
		{name: "folded", value: ">-"},
		{name: "explicit literal", value: "|2-"},
		{name: "explicit folded", value: ">2-"},
	}
	for _, layout := range layouts {
		t.Run(layout.name, func(t *testing.T) {
			for _, style := range styles {
				t.Run(style.name, func(t *testing.T) {
					for _, enabled := range []bool{true, false} {
						name := "enabled"
						if !enabled {
							name = "disabled"
						}
						t.Run(name, func(t *testing.T) {
							body := layout.anchorPrefix + style.value + "\n" +
								layout.tokenIndent + "${RPC_TOKEN_SEQUENCE_ALIAS}\n" +
								strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", "*rpc_token", 1)
							if !enabled {
								body = strings.Replace(body, "enabled: true", "enabled: false", 1)
							}
							cfg, err := Load(writeTempYAML(t, body))
							require.NoError(t, err)
							assert.Equal(t, token, cfg.InternalRPC.ClientToken)
							assert.Equal(t, enabled, cfg.InternalRPC.Enabled)
						})
					}
				})
			}
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

func TestInternalRPCTokenEnvironmentPreservesLegacyIndentation(t *testing.T) {
	token := `test_"\credential`
	t.Setenv("RPC_TOKEN_INDENT_TOKEN", token)
	layouts := []struct {
		name   string
		model  string
		limits string
	}{
		{
			name:   "indentless sequence",
			model:  "  - name: gpt-test\n    max_context_tokens: ${RPC_TOKEN_INDENT_LIMITS}",
			limits: "10000\n    max_completion_tokens: 1000",
		},
		{
			name:   "indented sequence",
			model:  "    - name: gpt-test\n      max_context_tokens: ${RPC_TOKEN_INDENT_LIMITS}",
			limits: "10000\n      max_completion_tokens: 1000",
		},
		{
			name:   "wide sequence indentation",
			model:  "      - name: gpt-test\n        max_context_tokens: ${RPC_TOKEN_INDENT_LIMITS}",
			limits: "10000\n        max_completion_tokens: 1000",
		},
	}
	profiles := []struct {
		name     string
		enabled  bool
		tokenEnv bool
	}{
		{name: "enabled literal tokens", enabled: true},
		{name: "enabled environment token", enabled: true, tokenEnv: true},
		{name: "disabled literal tokens"},
		{name: "disabled environment token", tokenEnv: true},
	}
	for _, layout := range layouts {
		t.Run(layout.name, func(t *testing.T) {
			t.Setenv("RPC_TOKEN_INDENT_LIMITS", layout.limits)
			for _, profile := range profiles {
				t.Run(profile.name, func(t *testing.T) {
					body := strings.Replace(plaintextInternalRPCYAML,
						"    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}",
						layout.model, 1)
					wantToken := "main_config_test_token"
					if profile.tokenEnv {
						body = strings.Replace(body, wantToken, `"${RPC_TOKEN_INDENT_TOKEN}"`, 1)
						wantToken = token
					}
					if !profile.enabled {
						body = strings.Replace(body, "enabled: true", "enabled: false", 1)
					}
					cfg, err := Load(writeTempYAML(t, body))
					require.NoError(t, err)
					assert.Equal(t, profile.enabled, cfg.InternalRPC.Enabled)
					assert.Equal(t, wantToken, cfg.InternalRPC.ClientToken)
					assert.Equal(t, 10000, cfg.OpenAI.Models[0].MaxContextTokens)
					assert.Equal(t, 1000, cfg.OpenAI.Models[0].MaxCompletionTokens)
				})
			}
		})
	}
}

func TestInternalRPCTokenEnvironmentScalarForms(t *testing.T) {
	token := `test_"\credential`
	t.Setenv("RPC_TOKEN_SCALAR_FORM", token)
	tests := []struct {
		name      string
		reference string
		prefix    string
		suffix    string
		flow      bool
	}{
		{name: "plain prefix and suffix", reference: "prefix_${RPC_TOKEN_SCALAR_FORM}_suffix", prefix: "prefix_", suffix: "_suffix"},
		{name: "quoted prefix and suffix", reference: `"prefix_${RPC_TOKEN_SCALAR_FORM}_suffix"`, prefix: "prefix_", suffix: "_suffix"},
		{name: "single quoted prefix and suffix", reference: "'prefix_${RPC_TOKEN_SCALAR_FORM}_suffix'", prefix: "prefix_", suffix: "_suffix"},
		{name: "tagged string", reference: `!!str "${RPC_TOKEN_SCALAR_FORM}"`},
		{name: "anchor before tag", reference: `&rpc_token !!str "${RPC_TOKEN_SCALAR_FORM}"`},
		{name: "tag before anchor", reference: `!!str &rpc_token "${RPC_TOKEN_SCALAR_FORM}"`},
		{name: "anchor with comment", reference: "&rpc_token # credential\n    \"${RPC_TOKEN_SCALAR_FORM}\""},
		{name: "literal block", reference: "|-\n    ${RPC_TOKEN_SCALAR_FORM}"},
		{name: "folded block", reference: ">-\n    ${RPC_TOKEN_SCALAR_FORM}"},
		{name: "explicit literal indentation", reference: "|2-\n    ${RPC_TOKEN_SCALAR_FORM}"},
		{name: "explicit folded indentation", reference: ">2-\n    ${RPC_TOKEN_SCALAR_FORM}"},
		{name: "block on next line", reference: "\n    |2-\n    ${RPC_TOKEN_SCALAR_FORM}"},
		{name: "flow mapping with unicode", reference: "${RPC_TOKEN_SCALAR_FORM}", flow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", tt.reference, 1)
			if tt.flow {
				body = strings.Split(plaintextInternalRPCYAML, "\ninternal_rpc:")[0] + `
internal_rpc: {enabled: true, security_mode: plaintext_token, listen: "127.0.0.1:9092", scheduler_endpoint: "127.0.0.1:9091", peer_tokens: {blowball-scheduler: scheduler_config_test_token}, 说明: 示例, client_token: ` + tt.reference + `}
`
			}
			cfg, err := Load(writeTempYAML(t, body))
			require.NoError(t, err)
			assert.Equal(t, tt.prefix+token+tt.suffix, cfg.InternalRPC.ClientToken)
		})
	}
}

func TestInternalRPCTokenEnvironmentLineBreaks(t *testing.T) {
	token := `test_"\credential`
	t.Setenv("RPC_TOKEN_LINE_BREAK", token)
	tests := []struct {
		name  string
		value string
	}{
		{name: "CRLF", value: "\r\n"},
		{name: "CR", value: "\r"},
		{name: "next line", value: "\u0085"},
		{name: "line separator", value: "\u2028"},
		{name: "paragraph separator", value: "\u2029"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(plaintextInternalRPCYAML, "main_config_test_token", `"${RPC_TOKEN_LINE_BREAK}"`, 1)
			body = strings.ReplaceAll(body, "\n", tt.value)
			cfg, err := Load(writeTempYAML(t, body))
			require.NoError(t, err)
			assert.Equal(t, token, cfg.InternalRPC.ClientToken)
		})
	}
}

func TestInternalRPCTokenEnvironmentLeadingBOM(t *testing.T) {
	token := `test_"\credential`
	t.Setenv("RPC_TOKEN_BOM", token)
	body := "\ufeffrpc_token: &rpc_token \"${RPC_TOKEN_BOM}\"\n" +
		strings.TrimPrefix(plaintextInternalRPCYAML, "\n")
	body = strings.Replace(body, "main_config_test_token", "*rpc_token", 1)
	cfg, err := Load(writeTempYAML(t, body))
	require.NoError(t, err)
	assert.Equal(t, token, cfg.InternalRPC.ClientToken)
}
