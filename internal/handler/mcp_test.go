package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// TestMCPTools_ReturnsXizhiAndInvokeTools verifies the combined shape: regular
// Xizhi tools from the registry + synthetic invoke_chongzhi / invoke_liang
// from agent.InvokeToolSchema.
func TestMCPTools_ReturnsXizhiAndInvokeTools(t *testing.T) {
	// Build a registry with all Xizhi tools scoped to a temp dir.
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, t.TempDir(), config.XizhiConfig{
		Read:      config.XizhiToolConfig{Enabled: true},
		Write:     config.XizhiToolConfig{Enabled: true},
		Modify:    config.XizhiToolConfig{Enabled: true},
		ListFiles: config.XizhiToolConfig{Enabled: true},
		Tree:      config.XizhiToolConfig{Enabled: true},
		GlobFiles: config.XizhiToolConfig{Enabled: true},
	})

	h := NewMCPHandler(reg)
	r := gin.New()
	r.GET("/api/v1/mcp/tools", h.Tools)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/tools", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Tools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// 6 Xizhi tools + 2 invoke tools = 8.
	require.Len(t, resp.Tools, 8, "expected 6 xizhi + 2 invoke tools")

	names := make(map[string]bool, len(resp.Tools))
	for _, t2 := range resp.Tools {
		names[t2.Name] = true
		assert.Equal(t, "function", t2.Type)
		assert.NotEmpty(t, t2.Description, "tool %q must have a description", t2.Name)
		assert.True(t, json.Valid(t2.Parameters), "tool %q parameters must be valid JSON", t2.Name)
	}

	assert.True(t, names[xizhi.NameReadFile], "xizhi_read_file must be present")
	assert.True(t, names[xizhi.NameWriteFile], "xizhi_write_file must be present")
	assert.True(t, names[xizhi.NameModifyFile], "xizhi_modify_file must be present")
	assert.True(t, names[xizhi.NameListFiles], "xizhi_list_files must be present")
	assert.True(t, names[xizhi.NameTree], "xizhi_tree must be present")
	assert.True(t, names[xizhi.NameGlobFiles], "xizhi_glob_files must be present")
	assert.True(t, names[agent.ToolInvokeChongzhi], "invoke_chongzhi must be present")
	assert.True(t, names[agent.ToolInvokeLiang], "invoke_liang must be present")
}
