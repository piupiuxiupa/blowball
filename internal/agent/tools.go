package agent

import (
	"encoding/json"
	"fmt"

	"github.com/lush/blowball/internal/tool"
)

// buildConfuciusToolsJSON returns the OpenAI tools[] JSON for the root agent.
// It merges the regular tools listed in cfg.Tools (resolved via the registry)
// with the synthetic spawn_subagent and root-only update_plan entries. Neither
// is registered in the tool registry — both are intercepted by the dispatch
// loop. Returns nil when the agent has no tools at all so callers can omit the
// field from the request.
// maxCompletionTokens is the TURN-resolved catalog-entry quota
// (per-model-completion-budget) behind the write-budget guidance number.
func buildConfuciusToolsJSON(reg *tool.Registry, regularToolNames []string, maxCompletionTokens int) ([]byte, error) {
	regularJSON, err := buildRegularToolsJSON(reg, regularToolNames, maxCompletionTokens)
	if err != nil {
		return nil, err
	}

	return combineRegularAndRootTools(regularJSON)
}

// buildSubAgentToolsJSON renders a generic sub-agent's narrowed tools and adds
// spawn_subagent only when another nesting level is allowed.
func buildSubAgentToolsJSON(reg *tool.Registry, regularToolNames []string, allowSpawn bool, maxCompletionTokens int) ([]byte, error) {
	regularJSON, err := buildRegularToolsJSON(reg, regularToolNames, maxCompletionTokens)
	if err != nil {
		return nil, err
	}
	return combineRegularAndSpawnTools(regularJSON, allowSpawn)
}

func combineRegularAndSpawnTools(regularJSON []byte, allowSpawn bool) ([]byte, error) {
	var regular openAIToolList
	if len(regularJSON) > 0 {
		if err := json.Unmarshal(regularJSON, &regular); err != nil {
			return nil, fmt.Errorf("agent: unmarshal regular tools: %w", err)
		}
	}
	if !allowSpawn {
		if len(regular) == 0 {
			return nil, nil
		}
		return json.Marshal(regular)
	}
	spawnTools := openAIToolList{
		{Type: "function", Function: openAIToolFunc{
			Name:        SpawnSubagentTool,
			Description: SpawnSubagentDescription,
			Parameters:  spawnArgsSchema,
		}},
	}

	if len(regularJSON) == 0 {
		return json.Marshal(spawnTools)
	}
	combined := append(regular, spawnTools...)
	return json.Marshal(combined)
}

// combineRegularAndRootTools adds the two root-only orchestration tools. Plan
// state is kept separate from spawn: children may recursively spawn when depth
// allows, but they can never observe or mutate the root plan.
func combineRegularAndRootTools(regularJSON []byte) ([]byte, error) {
	spawnJSON, err := combineRegularAndSpawnTools(regularJSON, true)
	if err != nil {
		return nil, err
	}
	var regular openAIToolList
	if err := json.Unmarshal(spawnJSON, &regular); err != nil {
		return nil, fmt.Errorf("agent: unmarshal root tools: %w", err)
	}
	regular = append(regular, openAITool{
		Type: "function",
		Function: openAIToolFunc{
			Name:        UpdatePlanTool,
			Description: UpdatePlanDescription,
			Parameters:  updatePlanArgsSchema,
		},
	})
	return json.Marshal(regular)
}

// buildRegularToolsJSON renders the OpenAI tools[] for an agent's plain tools
// (xizhi_*), with the write-budget guidance injected into the write-family
// tool descriptions (llm-length-continuation prevention side, design D10).
// The budget number's source is the TURN-resolved catalog entry's
// max_completion_tokens (per-model-completion-budget D4) — the registry is
// process-wide while the quota is per-turn/per-model, so the number can only
// be computed here, at the agent's render time. Returns nil when names is
// empty so the caller can omit Tools from the LLM request entirely.
func buildRegularToolsJSON(reg *tool.Registry, names []string, maxCompletionTokens int) ([]byte, error) {
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
	injectWriteBudgetGuidance(tools, maxCompletionTokens)
	out, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal regular tools: %w", err)
	}
	return out, nil
}

// writeBudgetGuidanceFraction is the fraction of the turn-resolved catalog
// entry's max_completion_tokens used as the per-single-write budget steering
// number (design D10): 70% leaves headroom for the arguments' JSON escaping
// inflation, any prose preamble, and parallel calls sharing one output
// budget. Applied as maxCompletionTokens*7/10 (NOT a precomputed 7/10
// constant — integer division would make it zero).
const writeBudgetGuidanceNumerator, writeBudgetGuidanceDenominator = 7, 10

// injectWriteBudgetGuidance appends the write-budget sentence to the
// xizhi_write_file / xizhi_modify_file descriptions with the number computed
// from the turn's resolved quota (floor(max_completion_tokens × 0.7),
// per-model-completion-budget D4 — anchored to the BASE quota, not quota +
// retries × step, because the guidance's semantics are "keep a single write
// clear of the cap"). The static ToolSpec descriptions already carry the
// pattern advice (split large content into multiple smaller writes); the
// injected sentence carries the concrete budget. A non-positive
// maxCompletionTokens (unresolved) skips the injection — there is no number
// to steer by. Tools an agent does not list are untouched.
func injectWriteBudgetGuidance(tools openAIToolList, maxCompletionTokens int) {
	budget := maxCompletionTokens * writeBudgetGuidanceNumerator / writeBudgetGuidanceDenominator
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
