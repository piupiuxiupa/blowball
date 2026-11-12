package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/store/mysql"
)

func TestInternalRPCOptionsPreserveSecurityMode(t *testing.T) {
	for _, mode := range []rpcx.SecurityMode{rpcx.SecurityModeMTLS, rpcx.SecurityModePlaintextToken} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := config.InternalRPCConfig{
				Enabled: true, SecurityMode: mode, Listen: "127.0.0.1:9092", SchedulerEndpoint: "127.0.0.1:9091",
				TLS:          config.TLSFilesConfig{CertFile: "/server.crt", KeyFile: "/server.key", CAFile: "/ca.crt"},
				SchedulerTLS: config.TLSFilesConfig{CertFile: "/client.crt", KeyFile: "/client.key", CAFile: "/ca.crt"},
				PeerTokens:   config.RPCPeerTokens{rpcx.IdentityScheduler: "peer_test_credential"}, ClientToken: "client_test_credential",
			}
			server, client := internalRPCOptions(cfg)
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
	logInternalRPCSecurity(log, cfg)
	require.Equal(t, 1, entries.Len())
	entry := entries.All()[0]
	assert.Equal(t, zapcore.WarnLevel, entry.Level)
	assert.Contains(t, entry.Message, "unencrypted")
	assert.Contains(t, entry.Message, "server identity")
	assert.NotContains(t, entry.Message, "MAY_LEAK")
	assert.Equal(t, cfg.Listen, entry.ContextMap()["listen"])
	assert.Equal(t, cfg.SchedulerEndpoint, entry.ContextMap()["scheduler_endpoint"])
	cfg.SecurityMode = rpcx.SecurityModeMTLS
	logInternalRPCSecurity(log, cfg)
	cfg.SecurityMode, cfg.Enabled = rpcx.SecurityModePlaintextToken, false
	logInternalRPCSecurity(log, cfg)
	assert.Equal(t, 1, entries.Len(), "mutual TLS and disabled RPC must not be labeled plaintext")
}

func TestInternalRPCOptionsDisabled(t *testing.T) {
	server, client := internalRPCOptions(config.InternalRPCConfig{
		SecurityMode: rpcx.SecurityModePlaintextToken, SchedulerEndpoint: "unused:9091", ClientToken: "unused_test_credential",
	})
	assert.Empty(t, server)
	assert.Empty(t, client)
}

func TestScheduledTurnServicesUseScheduledTableGroup(t *testing.T) {
	cfg := &config.Config{OpenAI: config.OpenAIConfig{Models: []config.ModelCatalogEntry{
		{Name: "test-model", MaxContextTokens: 128000, MaxCompletionTokens: 4096},
	}}}
	rt := &appRuntime{cfg: cfg, mysqlStore: &mysql.Store{}}
	services := newScheduledTurnServices(rt, nil, config.OpenAIConfig{}, 128000)

	require.NotNil(t, services.Sessions)
	require.NotNil(t, services.Messages)
	require.NotNil(t, services.Titles)
	require.NotNil(t, services.Compaction)
	require.NotNil(t, services.Snapshots)
	assert.Equal(t, 128000, services.Compaction.FallbackMaxContext())

	snapshots, ok := services.Snapshots.(*mysql.SessionStore)
	require.True(t, ok)
	assert.Equal(t, model.SessionSourceScheduled, snapshots.Source())
}
