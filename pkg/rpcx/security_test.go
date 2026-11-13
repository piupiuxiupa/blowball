package rpcx

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSecurityMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    SecurityMode
		want    SecurityMode
		wantErr bool
	}{
		{name: "omitted defaults to mutual TLS", want: SecurityModeMTLS},
		{name: "mutual TLS", mode: SecurityModeMTLS, want: SecurityModeMTLS},
		{name: "explicit plaintext token", mode: SecurityModePlaintextToken, want: SecurityModePlaintextToken},
		{name: "anonymous mode rejected", mode: "plaintext", wantErr: true},
		{name: "automatic negotiation rejected", mode: "auto", wantErr: true},
		{name: "case is not silently normalized", mode: "MTLS", wantErr: true},
		{name: "whitespace is not silently trimmed", mode: " mtls ", wantErr: true},
		{name: "unknown value is not echoed", mode: "MAY_LEAK_SECRET_mode", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeSecurityMode(tt.mode)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
				assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidatePeerTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tokens  map[string]string
		wantErr bool
	}{
		{name: "nil mapping", wantErr: true},
		{name: "empty mapping", tokens: map[string]string{}, wantErr: true},
		{name: "one permitted peer", tokens: map[string]string{IdentityMain: "main_test_token"}},
		{name: "all service identities", tokens: map[string]string{
			IdentityScheduler: "scheduler_test_token",
			IdentityAPI:       "api_test_token",
			IdentityAgent:     "agent_test_token",
			IdentityMain:      "main_test_token",
		}},
		{name: "unknown identity", tokens: map[string]string{"MAY_LEAK_SECRET_identity": "main_test_token"}, wantErr: true},
		{name: "empty token", tokens: map[string]string{IdentityMain: ""}, wantErr: true},
		{name: "whitespace token", tokens: map[string]string{IdentityMain: "MAY_LEAK_SECRET invalid"}, wantErr: true},
		{name: "control character", tokens: map[string]string{IdentityMain: "MAY_LEAK_SECRET\x00"}, wantErr: true},
		{name: "non ASCII", tokens: map[string]string{IdentityMain: "MAY_LEAK_SECRET_秘密"}, wantErr: true},
		{name: "ambiguous identities", tokens: map[string]string{
			IdentityMain:  "MAY_LEAK_SECRET_shared",
			IdentityAgent: "MAY_LEAK_SECRET_shared",
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(tt.tokens)
			err := ValidatePeerTokens(tt.tokens)
			assert.Equal(t, before, tt.tokens, "validation must not mutate caller configuration")
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateClientToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "empty", wantErr: true},
		{name: "space", token: " ", wantErr: true},
		{name: "leading whitespace", token: " MAY_LEAK_SECRET", wantErr: true},
		{name: "embedded whitespace", token: "MAY_LEAK_SECRET invalid", wantErr: true},
		{name: "tab", token: "MAY_LEAK_SECRET\t", wantErr: true},
		{name: "newline", token: "MAY_LEAK_SECRET\n", wantErr: true},
		{name: "NUL", token: "MAY_LEAK_SECRET\x00", wantErr: true},
		{name: "other control", token: "MAY_LEAK_SECRET\x1f", wantErr: true},
		{name: "DEL", token: "MAY_LEAK_SECRET\x7f", wantErr: true},
		{name: "non ASCII whitespace", token: "MAY_LEAK_SECRET\u00a0", wantErr: true},
		{name: "non ASCII letters", token: "MAY_LEAK_SECRET_秘密", wantErr: true},
		{name: "opaque ASCII", token: "test_token-._~+/=0123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateClientToken(tt.token)
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
				return
			}
			require.NoError(t, err)
		})
	}
}
