package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInvokeToolDescription_KnownAndUnknown pins the single exported source for
// the synthetic invoke_* tool descriptions, so the model-facing tools[] array
// and the MCP catalogue cannot drift (delta spec change "eliminate H1 double-write").
func TestInvokeToolDescription_KnownAndUnknown(t *testing.T) {
	assert.Equal(t, InvokeChongzhiDescription, InvokeToolDescription(ToolInvokeChongzhi))
	assert.Equal(t, InvokeLiangDescription, InvokeToolDescription(ToolInvokeLiang))
	assert.NotEmpty(t, InvokeToolDescription(ToolInvokeChongzhi))
	assert.NotEmpty(t, InvokeToolDescription(ToolInvokeLiang))
	assert.Empty(t, InvokeToolDescription("invoke_unknown"))
}

// TestBuildConfuciusToolsJSON_InvokeDescriptionsFromSingleSource asserts that
// buildConfuciusToolsJSON renders the descriptions produced by
// InvokeToolDescription rather than a second hardcoded copy.
func TestBuildConfuciusToolsJSON_InvokeDescriptionsFromSingleSource(t *testing.T) {
	data, err := buildConfuciusToolsJSON(nil, nil)
	require.NoError(t, err)

	var tools []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	}
	require.NoError(t, json.Unmarshal(data, &tools))

	descByName := make(map[string]string, len(tools))
	for _, t2 := range tools {
		descByName[t2.Function.Name] = t2.Function.Description
	}
	assert.Equal(t, InvokeToolDescription(ToolInvokeChongzhi), descByName[ToolInvokeChongzhi])
	assert.Equal(t, InvokeToolDescription(ToolInvokeLiang), descByName[ToolInvokeLiang])
}
