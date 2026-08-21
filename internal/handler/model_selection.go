package handler

import (
	"strconv"
	"strings"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"go.uber.org/zap"
)

// ModelSelectionConfig carries the deployment's model-catalog state into the
// streaming handler (per-request-model-selection, dual-axis form of
// model-effort-v2): it resolves every chat request's `model`/`reasoning_effort`
// pair into the turn's (model, effort) — always, since the catalog is
// mandatory — and fixes the per-turn compaction threshold and the model name
// recorded in run meta and turn_usage. Build it with NewModelSelectionConfig
// at wiring time; the zero value carries no catalog and rejects every request
// (resolution cannot find the default entry), which only hand-built tests can
// produce.
type ModelSelectionConfig struct {
	// Catalog is the effective model catalog (the mandatory openai.models
	// list).
	Catalog []config.ModelCatalogEntry
	// Default is the default model name (openai.default_model / first
	// entry) — the model axis's fallback.
	Default string
	// DefaultEffort is the deployment-level default reasoning effort
	// (openai.default_reasoning_effort, normalized to "none" when unset) —
	// the effort axis's fallback.
	DefaultEffort string
	// AnyOutputSchema is true when any agent configures output_schema. A
	// turn resolving to effort != none conflicts with structured output
	// (reasoning models do not support it) and is rejected with 400 rather
	// than silently degrading — the runtime twin of the config-level
	// default-effort × output_schema load check.
	AnyOutputSchema bool
}

// NewModelSelectionConfig derives the handler's model-selection state from a
// loaded config. Every turn resolves against this state — parameter-less
// requests take the default entry plus the deployment default effort.
func NewModelSelectionConfig(cfg *config.Config) ModelSelectionConfig {
	if cfg == nil {
		return ModelSelectionConfig{}
	}
	msc := ModelSelectionConfig{
		Catalog: cfg.ModelCatalog(),
		Default: cfg.DefaultModelName(),
		AnyOutputSchema: anyOutputSchema(
			cfg.Agents.Confucius.OutputSchema, cfg.Agents.Chongzhi.OutputSchema, cfg.Agents.Liang.OutputSchema),
	}
	// Config load normalizes an unset default_reasoning_effort to "none";
	// hand-built configs (tests) get the same normalization here so the
	// closed-set validation below never trips on an empty fallback.
	msc.DefaultEffort = strings.TrimSpace(cfg.OpenAI.DefaultReasoningEffort)
	if msc.DefaultEffort == "" {
		msc.DefaultEffort = "none"
	}
	return msc
}

// anyOutputSchema reports whether any of the three agents configures an
// output_schema (the effort != none conflict gate).
func anyOutputSchema(schemas ...string) bool {
	for _, s := range schemas {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// modelSelection is the resolution result of one chat request's model
// parameters (model-effort-v2's two independent axes). It is ALWAYS fully
// resolved — there is no "parameters absent" zero state anymore; a
// parameter-less request resolves to the default entry + deployment default
// effort.
type modelSelection struct {
	// Model is the resolved catalog entry's name (request `model` or the
	// default entry).
	Model string
	// Thinking is the resolved entry's wire-family marker (a copy of
	// entry.Thinking): true → every LLM call of the turn sends
	// reasoning_effort (literal none included) plus max_completion_tokens;
	// false → no reasoning_effort, max_tokens.
	Thinking bool
	// Effort is the effective effort level (request `reasoning_effort` or
	// the deployment default), clamped to "none" on non-thinking entries.
	Effort string
	// MaxContextTokens is the resolved entry's context window (the turn's
	// compaction threshold).
	MaxContextTokens int
	// MaxCompletionTokens is the resolved entry's output-token quota
	// (per-model-completion-budget): shared by every LLM call of the turn's
	// three agents (wrap-up rounds included).
	MaxCompletionTokens int
	// LengthContinue is the resolved entry's finish_reason=length
	// continuation policy: zero when the entry does not configure the
	// sub-block (continuation off for that model).
	LengthContinue config.LengthContinueConfig
}

// override renders the selection as the agent-layer turn config. The value is
// always non-zero — Build injects it into every agent of the turn.
func (s modelSelection) override() agent.ModelOverride {
	return agent.ModelOverride{
		Model:               s.Model,
		Thinking:            s.Thinking,
		ReasoningEffort:     s.Effort,
		MaxCompletionTokens: s.MaxCompletionTokens,
		LengthContinue:      s.LengthContinue,
	}
}

// requestEfforts is the closed set of accepted reasoning_effort values
// (request parameter and deployment default alike).
var requestEfforts = map[string]bool{
	"none": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// resolveModelSelection applies model-effort-v2's two independent axes to one
// chat request:
//
//	model  = request.model            || default catalog entry
//	effort = request.reasoning_effort || openai.default_reasoning_effort
//
//	model    unknown to the catalog                 → 400 INVALID_MODEL
//	effort   outside none|low|medium|high|xhigh|max → 400 INVALID_EFFORT
//	effort   != none, explicitly requested, on a
//	         thinking:false entry                    → 400 INVALID_EFFORT
//	effort   != none via the DEPLOYMENT default on a
//	         thinking:false entry                    → clamped to none + WARN
//	any output_schema agent + resolved effort != none → 400 INVALID_EFFORT
//
// The returned error code/message are empty on success.
func resolveModelSelection(msc ModelSelectionConfig, model, effort string) (modelSelection, string, string) {
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)

	// Model axis: the named model, or the default entry when the request
	// carried none.
	name := model
	if name == "" {
		name = msc.Default
	}
	entry, ok := findEntry(msc.Catalog, name)
	if !ok {
		return modelSelection{}, "INVALID_MODEL", "unknown model " + strconv.Quote(name) + ": not in the configured openai.models catalog"
	}

	// Effort axis: the requested level or the deployment default, then gate
	// on the entry's thinking capability. An explicit non-none request on a
	// non-thinking entry is a client error; the deployment default landing
	// there is a config-level mismatch that cannot be caught at load (the
	// default entry may think while others do not) — clamp it to none and
	// WARN so the turn still runs.
	effective := effort
	if effective == "" {
		effective = msc.DefaultEffort
	}
	if !requestEfforts[effective] {
		return modelSelection{}, "INVALID_EFFORT", "invalid reasoning_effort " + strconv.Quote(effective) + " (must be none, low, medium, high, xhigh or max)"
	}
	if effective != "none" && !entry.Thinking {
		if effort != "" {
			return modelSelection{}, "INVALID_EFFORT", "model " + strconv.Quote(entry.Name) + " does not support reasoning; only reasoning_effort none is allowed"
		}
		logger.L().Warn("deployment default reasoning_effort clamped to none on a non-thinking catalog entry",
			zap.String("op", "handler.resolve_model_selection"),
			zap.String("model", entry.Name),
			zap.String("default_reasoning_effort", msc.DefaultEffort))
		effective = "none"
	}

	// Structured-output conflict: an output_schema agent cannot run a
	// reasoning turn (config-level gate); fail fast instead of degrading.
	if msc.AnyOutputSchema && effective != "none" {
		return modelSelection{}, "INVALID_EFFORT", "reasoning_effort " + strconv.Quote(effective) + " conflicts with a configured agent output_schema (reasoning models do not support structured output)"
	}

	return modelSelection{
		Model:               entry.Name,
		Thinking:            entry.Thinking,
		Effort:              effective,
		MaxContextTokens:    entry.MaxContextTokens,
		MaxCompletionTokens: entry.MaxCompletionTokens,
		LengthContinue:      entry.LengthContinue,
	}, "", ""
}

// findEntry resolves a name against the catalog.
func findEntry(catalog []config.ModelCatalogEntry, name string) (config.ModelCatalogEntry, bool) {
	for _, e := range catalog {
		if e.Name == name {
			return e, true
		}
	}
	return config.ModelCatalogEntry{}, false
}
