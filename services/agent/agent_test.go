package agentserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/store/mysql"
)

func TestScheduledTurnServicesUseScheduledTableGroup(t *testing.T) {
	cfg := &config.Config{OpenAI: config.OpenAIConfig{Models: []config.ModelCatalogEntry{
		{Name: "test-model", MaxContextTokens: 128000, MaxCompletionTokens: 4096},
	}}}
	rt := &boot.Runtime{Cfg: cfg, MySQLStore: &mysql.Store{}}
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
