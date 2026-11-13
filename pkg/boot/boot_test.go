package boot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/rpcx"
)

// TestOpenAIKeyRequired verifies the role-aware openai.api_key requirement.
// Only the api role is exempt: it never constructs the OpenAI client or
// orchestrator, so it must not be gated on an LLM key at startup. The all and
// agent roles build the agent layer and therefore require the key. This is the
// decision-level enforcement of the service-roles spec's fault-isolation
// requirement that "the api role does not depend on the agent layer".
func TestOpenAIKeyRequired(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"api", false},
		{"all", true},
		{"agent", true},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			if got := OpenAIKeyRequired(tc.role); got != tc.want {
				t.Errorf("OpenAIKeyRequired(%q) = %v, want %v", tc.role, got, tc.want)
			}
		})
	}
}

// TestRouteGroups verifies the startup-log route partition names per role.
func TestRouteGroups(t *testing.T) {
	cases := []struct {
		role string
		want []string
	}{
		{"api", []string{"api"}},
		{"agent", []string{"agent"}},
		{"all", []string{"api", "agent"}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got := RouteGroups(tc.role)
			if len(got) != len(tc.want) {
				t.Fatalf("RouteGroups(%q) = %v, want %v", tc.role, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("RouteGroups(%q)[%d] = %q, want %q", tc.role, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestInternalRPCOptionsPreserveSecurityMode(t *testing.T) {
	for _, mode := range []rpcx.SecurityMode{rpcx.SecurityModeMTLS, rpcx.SecurityModePlaintextToken} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := config.InternalRPCConfig{
				Enabled: true, SecurityMode: mode, Listen: "127.0.0.1:9092", SchedulerEndpoint: "127.0.0.1:9091",
				TLS:          config.TLSFilesConfig{CertFile: "/server.crt", KeyFile: "/server.key", CAFile: "/ca.crt"},
				SchedulerTLS: config.TLSFilesConfig{CertFile: "/client.crt", KeyFile: "/client.key", CAFile: "/ca.crt"},
				PeerTokens:   config.RPCPeerTokens{rpcx.IdentityScheduler: "peer_test_credential"}, ClientToken: "client_test_credential",
			}
			server, client := InternalRPCOptions(cfg)
			assert.Equal(t, mode, server.SecurityMode)
			assert.Equal(t, mode, client.SecurityMode)
			assert.Equal(t, cfg.TLS.ToRPCX(), server.TLS)
			assert.Equal(t, cfg.SchedulerTLS.ToRPCX(), client.TLS)
			assert.Equal(t, cfg.SchedulerEndpoint, client.Target)
			assert.Equal(t, cfg.ClientToken, client.ClientToken)
			assert.Equal(t, map[string]string(cfg.PeerTokens), server.PeerTokens)
			assert.True(t, server.RequireDeadline)
		})
	}
}

func TestInternalRPCSecurityWarning(t *testing.T) {
	core, entries := observer.New(zapcore.DebugLevel)
	log := zap.New(core)
	cfg := config.InternalRPCConfig{Enabled: true, SecurityMode: rpcx.SecurityModePlaintextToken,
		Listen: "127.0.0.1:9092", SchedulerEndpoint: "127.0.0.1:9091", ClientToken: "MAY_LEAK_SECRET_outbound",
		PeerTokens: config.RPCPeerTokens{rpcx.IdentityScheduler: "MAY_LEAK_SECRET_inbound"},
	}
	LogInternalRPCSecurity(log, cfg)
	require.Equal(t, 1, entries.Len())
	entry := entries.All()[0]
	assert.Equal(t, zapcore.WarnLevel, entry.Level)
	assert.Contains(t, entry.Message, "unencrypted")
	assert.Contains(t, entry.Message, "server identity")
	assert.NotContains(t, entry.Message, "MAY_LEAK")
	assert.Equal(t, cfg.Listen, entry.ContextMap()["listen"])
	assert.Equal(t, cfg.SchedulerEndpoint, entry.ContextMap()["scheduler_endpoint"])
	cfg.SecurityMode = rpcx.SecurityModeMTLS
	LogInternalRPCSecurity(log, cfg)
	cfg.SecurityMode, cfg.Enabled = rpcx.SecurityModePlaintextToken, false
	LogInternalRPCSecurity(log, cfg)
	assert.Equal(t, 1, entries.Len(), "mutual TLS and disabled RPC must not be labeled plaintext")
}

func TestInternalRPCOptionsDisabled(t *testing.T) {
	server, client := InternalRPCOptions(config.InternalRPCConfig{
		SecurityMode: rpcx.SecurityModePlaintextToken, SchedulerEndpoint: "unused:9091", ClientToken: "unused_test_credential",
	})
	assert.Empty(t, server)
	assert.Empty(t, client)
}
