package main

import (
	"testing"

	mainconfig "github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/services/scheduler/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSchedulerRPCOptionsPreserveSecurityMode(t *testing.T) {
	for _, mode := range []rpcx.SecurityMode{rpcx.SecurityModeMTLS, rpcx.SecurityModePlaintextToken} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := config.Config{
				RPC: config.RPCConfig{SecurityMode: mode,
					TLS:        rpcx.TLSFiles{CertFile: "/server.crt", KeyFile: "/server.key", CAFile: "/ca.crt"},
					PeerTokens: mainconfig.RPCPeerTokens{rpcx.IdentityMain: "main_test_credential"},
				},
				MainService: config.MainServiceConfig{APIEndpoint: "api:9092", AgentEndpoint: "agent:9093",
					ClientToken: "scheduler_test_credential",
					TLS:         rpcx.TLSFiles{CertFile: "/client.crt", KeyFile: "/client.key", CAFile: "/ca.crt"},
				},
				Scheduler: config.SchedulerConfig{ExecutionEnabled: new(false)},
			}
			for _, endpoint := range []string{cfg.MainService.APIEndpoint, cfg.MainService.AgentEndpoint} {
				server, client := schedulerRPCOptions(cfg, endpoint)
				assert.Equal(t, mode, server.SecurityMode)
				assert.Equal(t, mode, client.SecurityMode)
				assert.Equal(t, cfg.RPC.TLS, server.TLS)
				assert.Equal(t, cfg.MainService.TLS, client.TLS)
				assert.Equal(t, cfg.MainService.ClientToken, client.ClientToken)
				assert.Equal(t, endpoint, client.Target)
				assert.Equal(t, map[string]string(cfg.RPC.PeerTokens), server.PeerTokens)
				assert.True(t, server.RequireDeadline)
			}
		})
	}
}

func TestSchedulerRPCSecurityWarning(t *testing.T) {
	core, entries := observer.New(zapcore.DebugLevel)
	log := zap.New(core)
	cfg := config.Config{
		RPC:         config.RPCConfig{SecurityMode: rpcx.SecurityModePlaintextToken, Listen: "127.0.0.1:9091"},
		MainService: config.MainServiceConfig{APIEndpoint: "api:9092", AgentEndpoint: "agent:9093", ClientToken: "MAY_LEAK_SECRET"},
	}
	logSchedulerRPCSecurity(log, cfg)
	require.Equal(t, 1, entries.Len())
	entry := entries.All()[0]
	assert.Equal(t, zapcore.WarnLevel, entry.Level)
	assert.Contains(t, entry.Message, "unencrypted")
	assert.Contains(t, entry.Message, "server identity")
	assert.NotContains(t, entry.Message, "MAY_LEAK")
	assert.Equal(t, cfg.RPC.Listen, entry.ContextMap()["listen"])
	assert.Equal(t, cfg.MainService.APIEndpoint, entry.ContextMap()["api_endpoint"])
	assert.Equal(t, cfg.MainService.AgentEndpoint, entry.ContextMap()["agent_endpoint"])
	cfg.RPC.SecurityMode = rpcx.SecurityModeMTLS
	logSchedulerRPCSecurity(log, cfg)
	assert.Equal(t, 1, entries.Len())
}
