package agent

import (
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/tool"
)

// buildConfuseToolsJSON returns the OpenAI tools[] JSON for the Confuse agent.
// It merges the regular tools listed in cfg.Tools (resolved via the registry)
// with the synthetic invoke_chongzhi / invoke_liang entries that Confuse uses
// to dispatch sub-agents. The invoke_* tools are NOT registered in the tool
// registry — they are intercepted by the Confuse Run loop. Returns nil when
// the agent has no tools at all so callers can omit the field from the request.
func buildConfuseToolsJSON(reg *tool.Registry, regularToolNames []string) ([]byte, error) {
	regularJSON, err := buildRegularToolsJSON(reg, regularToolNames)
	if err != nil {
		return nil, err
	}

	invokeTools := openAIToolList{
		{Type: "function", Function: openAIToolFunc{
			Name:        ToolInvokeChongzhi,
			Description: "Invoke the Chongzhi (coding) sub-agent. Use for code editing, file writing, or any task that requires modifying files in the user's workspace.",
			Parameters:  invokeArgsSchema,
		}},
		{Type: "function", Function: openAIToolFunc{
			Name:        ToolInvokeLiang,
			Description: "Invoke the Liang (analysis) sub-agent. Use for analysis, explanation, or reasoning tasks that do not require file modification.",
			Parameters:  invokeArgsSchema,
		}},
	}

	if len(regularJSON) == 0 {
		return json.Marshal(invokeTools)
	}

	var regular openAIToolList
	if err := json.Unmarshal(regularJSON, &regular); err != nil {
		return nil, fmt.Errorf("agent: unmarshal regular tools: %w", err)
	}
	combined := append(regular, invokeTools...)
	return json.Marshal(combined)
}

// buildRegularToolsJSON renders the OpenAI tools[] for an agent's plain tools
// (xizhi_*). Returns nil when names is empty so the caller can omit Tools
// from the LLM request entirely.
func buildRegularToolsJSON(reg *tool.Registry, names []string) ([]byte, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if reg == nil {
		return nil, fmt.Errorf("agent: tool registry is nil but agent has %d tools configured", len(names))
	}
	return reg.OpenAITools(names)
}

// openAIToolList and openAIToolFunc mirror the OpenAI tools[] wire shape so we
// can render / parse agent tools without importing openai-go in the helper.
type openAIToolList []openAITool

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIToolFunc `json:"function"`
}

type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}
