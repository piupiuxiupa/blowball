package agent

import (
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/tool"
)

// buildConfuciusToolsJSON returns the OpenAI tools[] JSON for the Confucius agent.
// It merges the regular tools listed in cfg.Tools (resolved via the registry)
// with the synthetic invoke_chongzhi / invoke_liang entries that Confucius uses
// to dispatch sub-agents. The invoke_* tools are NOT registered in the tool
// registry — they are intercepted by the Confucius Run loop. Returns nil when
// the agent has no tools at all so callers can omit the field from the request.
func buildConfuciusToolsJSON(reg *tool.Registry, regularToolNames []string, maxTokens int) ([]byte, error) {
	regularJSON, err := buildRegularToolsJSON(reg, regularToolNames, maxTokens)
	if err != nil {
		return nil, err
	}

	invokeTools := openAIToolList{
		{Type: "function", Function: openAIToolFunc{
			Name:        ToolInvokeChongzhi,
			Description: InvokeToolDescription(ToolInvokeChongzhi),
			Parameters:  invokeArgsSchema,
		}},
		{Type: "function", Function: openAIToolFunc{
			Name:        ToolInvokeLiang,
			Description: InvokeToolDescription(ToolInvokeLiang),
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
// (xizhi_*), with the per-agent write-budget guidance injected into the
// write-family tool descriptions (llm-length-continuation prevention side,
// design D10 — the registry is process-wide while max_tokens is per-agent, so
// the budget number can only be computed here, at the agent's render time).
// Returns nil when names is empty so the caller can omit Tools from the LLM
// request entirely.
func buildRegularToolsJSON(reg *tool.Registry, names []string, maxTokens int) ([]byte, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if reg == nil {
		return nil, fmt.Errorf("agent: tool registry is nil but agent has %d tools configured", len(names))
	}
	raw, err := reg.OpenAITools(names)
	if err != nil {
		return nil, err
	}
	var tools openAIToolList
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("agent: unmarshal regular tools: %w", err)
	}
	injectWriteBudgetGuidance(tools, maxTokens)
	out, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal regular tools: %w", err)
	}
	return out, nil
}

// writeBudgetGuidanceFraction is the fraction of the agent's configured
// max_tokens used as the per-single-write budget steering number (design
// D10): 70% leaves headroom for the arguments' JSON escaping inflation, any
// prose preamble, and parallel calls sharing one output budget. Applied as
// maxTokens*7/10 (NOT a precomputed 7/10 constant — integer division would
// make it zero).
const writeBudgetGuidanceNumerator, writeBudgetGuidanceDenominator = 7, 10

// injectWriteBudgetGuidance appends the write-budget sentence to the
// xizhi_write_file / xizhi_modify_file descriptions with the number computed
// from THIS agent's max_tokens (floor(max_tokens × 0.7)). The static ToolSpec
// descriptions already carry the pattern advice (split large content into
// multiple smaller writes); the injected sentence carries the concrete
// budget. A non-positive maxTokens (unconfigured) skips the injection — there
// is no number to steer by. Tools an agent does not list are untouched.
func injectWriteBudgetGuidance(tools openAIToolList, maxTokens int) {
	budget := maxTokens * writeBudgetGuidanceNumerator / writeBudgetGuidanceDenominator
	if budget <= 0 {
		return
	}
	suffix := fmt.Sprintf(" **Write budget:** keep the content of a single write under ~%d tokens. For larger content, split it into multiple smaller writes (first write, then continuation writes) instead of producing one huge output.", budget)
	for i := range tools {
		switch tools[i].Function.Name {
		case "xizhi_write_file", "xizhi_modify_file":
			tools[i].Function.Description += suffix
		}
	}
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
