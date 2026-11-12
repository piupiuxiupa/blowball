package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/store/mysql"
)

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
