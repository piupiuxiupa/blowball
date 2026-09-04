package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildConfuciusToolsJSON_RootSyntheticTools(t *testing.T) {
	data, err := buildConfuciusToolsJSON(nil, nil, 0)
	require.NoError(t, err)

	var tools []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal(data, &tools))
	require.Len(t, tools, 2)
	assert.Equal(t, SpawnSubagentTool, tools[0].Function.Name)
	assert.Equal(t, SpawnSubagentDescription, tools[0].Function.Description)
	assert.Equal(t, UpdatePlanTool, tools[1].Function.Name)
	assert.Equal(t, UpdatePlanDescription, tools[1].Function.Description)
}

func TestBuildSubAgentToolsJSON_DepthControlsSpawn(t *testing.T) {
	withSpawn, err := buildSubAgentToolsJSON(nil, nil, true, 100)
	require.NoError(t, err)
	assert.Contains(t, string(withSpawn), SpawnSubagentTool)
	assert.NotContains(t, string(withSpawn), UpdatePlanTool)

	withoutSpawn, err := buildSubAgentToolsJSON(nil, nil, false, 100)
	require.NoError(t, err)
	assert.NotContains(t, string(withoutSpawn), SpawnSubagentTool)
	assert.NotContains(t, string(withoutSpawn), UpdatePlanTool)
}
