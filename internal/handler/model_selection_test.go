package handler

import (
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selectionTestCatalog is a two-entry catalog — a thinking model (the
// default) and a non-thinking one — with the deployment default effort unset
// (normalizing to none inside NewModelSelectionConfig; hand-built configs
// normalize the same way inside resolveModelSelection). The entries carry
// distinct output quotas and only the default entry configures
// length_continue, so per-entry quota/continuation flow is observable
// (per-model-completion-budget).
func selectionTestCatalog() ModelSelectionConfig {
	return ModelSelectionConfig{
		Catalog: []config.ModelCatalogEntry{
			{Name: "gpt-5", MaxContextTokens: 400000, MaxCompletionTokens: 16384, Thinking: true,
				LengthContinue: config.LengthContinueConfig{ExpandStep: 8192, MaxRetries: 2}},
			{Name: "glm-4.7", MaxContextTokens: 200000, MaxCompletionTokens: 8192, Thinking: false},
		},
		Default:       "gpt-5",
		DefaultEffort: "none",
	}
}

// requireResolve asserts the resolution succeeds and returns the selection.
func requireResolve(t *testing.T, msc ModelSelectionConfig, model, effort string) modelSelection {
	t.Helper()
	sel, code, msg := resolveModelSelection(msc, model, effort)
	require.Emptyf(t, code, "unexpected error %s: %s", code, msg)
	return sel
}

// requireReject asserts the resolution fails with the given code.
func requireReject(t *testing.T, msc ModelSelectionConfig, model, effort, wantCode string) {
	t.Helper()
	sel, code, msg := resolveModelSelection(msc, model, effort)
	require.NotEmptyf(t, code, "expected error %s, got selection %+v", wantCode, sel)
	assert.Equal(t, wantCode, code)
	assert.NotEmpty(t, msg)
}

// TestResolveModelSelection_NoParameters covers the both-axes-default case:
// no model, no effort → the default entry + deployment default effort, always
// a fully-resolved selection (model-effort-v2: there is no "parameters
// absent" zero state).
func TestResolveModelSelection_NoParameters(t *testing.T) {
	sel := requireResolve(t, selectionTestCatalog(), "", "")
	assert.Equal(t, "gpt-5", sel.Model)
	assert.True(t, sel.Thinking, "wire family mirrors the entry's thinking capability")
	assert.Equal(t, "none", sel.Effort, "unset deployment default resolves to none")
	assert.Equal(t, 400000, sel.MaxContextTokens)
	ov := sel.override()
	assert.Equal(t, "gpt-5", ov.Model)
	assert.True(t, ov.Thinking)
}

// TestResolveModelSelection_DeploymentDefaultEffort: a configured
// openai.default_reasoning_effort feeds parameter-less turns (the effort
// axis's only fallback).
func TestResolveModelSelection_DeploymentDefaultEffort(t *testing.T) {
	msc := selectionTestCatalog()
	msc.DefaultEffort = "high"
	sel := requireResolve(t, msc, "", "")
	assert.Equal(t, "high", sel.Effort)
	assert.Equal(t, "high", sel.override().ReasoningEffort)
}

// TestResolveModelSelection_EffortOnly covers the effort axis alone: the
// request effort applies to the DEFAULT entry.
func TestResolveModelSelection_EffortOnly(t *testing.T) {
	msc := selectionTestCatalog()

	// Default model thinks → effort applies verbatim; effort=none on a
	// thinking entry stays LITERAL (B2): the wire family stays thinking and
	// the override carries "none" for the client to send.
	sel := requireResolve(t, msc, "", "low")
	assert.Equal(t, "gpt-5", sel.Model)
	assert.True(t, sel.Thinking)
	assert.Equal(t, "low", sel.Effort)

	sel = requireResolve(t, msc, "", "none")
	assert.True(t, sel.Thinking, "none does not flip the wire family")
	assert.Equal(t, "none", sel.Effort)
	ov := sel.override()
	assert.True(t, ov.Thinking)
	assert.Equal(t, "none", ov.ReasoningEffort, "none rides as a literal value for thinking entries")
}

// TestResolveModelSelection_ModelOnly covers the model axis alone: the
// effort comes from the deployment default (NOT a hard-coded medium), and a
// non-thinking entry clamps that default to none.
func TestResolveModelSelection_ModelOnly(t *testing.T) {
	msc := selectionTestCatalog()

	sel := requireResolve(t, msc, "gpt-5", "")
	assert.Equal(t, "gpt-5", sel.Model)
	assert.Equal(t, "none", sel.Effort, "unset default effort resolves to none (not medium)")

	sel = requireResolve(t, msc, "glm-4.7", "")
	assert.Equal(t, "glm-4.7", sel.Model)
	assert.False(t, sel.Thinking)
	assert.Equal(t, "none", sel.Effort)
	assert.Equal(t, 200000, sel.MaxContextTokens)

	// With a non-none deployment default, a model-only request picks the
	// default entry AND the default effort.
	hi := selectionTestCatalog()
	hi.DefaultEffort = "xhigh"
	sel = requireResolve(t, hi, "", "")
	assert.Equal(t, "xhigh", sel.Effort)
}

// TestResolveModelSelection_PerEntryQuotaAndContinuation: the resolved
// entry's output quota and length_continue policy ride the selection and its
// override verbatim — one deployment, two entries, each turn gets its own
// pair (per-model-completion-budget).
func TestResolveModelSelection_PerEntryQuotaAndContinuation(t *testing.T) {
	msc := selectionTestCatalog()

	sel := requireResolve(t, msc, "gpt-5", "")
	assert.Equal(t, 16384, sel.MaxCompletionTokens)
	assert.True(t, sel.LengthContinue.Enabled(), "entry-configured continuation must ride the selection")
	step, retries := sel.LengthContinue.Resolve()
	assert.Equal(t, 8192, step)
	assert.Equal(t, 2, retries)
	ov := sel.override()
	assert.Equal(t, 16384, ov.MaxCompletionTokens, "override carries the entry quota")
	assert.Equal(t, sel.LengthContinue, ov.LengthContinue, "override carries the entry continuation policy")

	sel = requireResolve(t, msc, "glm-4.7", "")
	assert.Equal(t, 8192, sel.MaxCompletionTokens)
	assert.False(t, sel.LengthContinue.Enabled(), "an entry without length_continue resolves disabled")
	ov = sel.override()
	assert.Equal(t, 8192, ov.MaxCompletionTokens)
	assert.False(t, ov.LengthContinue.Enabled())
}

// TestResolveModelSelection_ModelAndEffort: model + effort — both axes apply
// independently; the entry gates the effort.
func TestResolveModelSelection_ModelAndEffort(t *testing.T) {
	msc := selectionTestCatalog()

	sel := requireResolve(t, msc, "glm-4.7", "none")
	assert.Equal(t, "glm-4.7", sel.Model)
	assert.False(t, sel.Thinking)

	sel = requireResolve(t, msc, "gpt-5", "max")
	assert.Equal(t, "gpt-5", sel.Model)
	assert.True(t, sel.Thinking)
	assert.Equal(t, "max", sel.Effort)
	ov := sel.override()
	assert.Equal(t, "max", ov.ReasoningEffort)
}

// TestResolveModelSelection_InvalidModel: unknown names are rejected with
// INVALID_MODEL.
func TestResolveModelSelection_InvalidModel(t *testing.T) {
	requireReject(t, selectionTestCatalog(), "gpt-99", "", "INVALID_MODEL")
	requireReject(t, selectionTestCatalog(), "gpt-99", "low", "INVALID_MODEL")
}

// TestResolveModelSelection_InvalidEffort: values outside the closed set, and
// non-"none" values EXPLICITLY REQUESTED on a thinking:false entry, are
// rejected with INVALID_EFFORT.
func TestResolveModelSelection_InvalidEffort(t *testing.T) {
	msc := selectionTestCatalog()

	// Not in the closed set (request or deployment default alike).
	requireReject(t, msc, "gpt-5", "ultra", "INVALID_EFFORT")
	requireReject(t, msc, "", "MINIMUM", "INVALID_EFFORT")
	badDefault := selectionTestCatalog()
	badDefault.DefaultEffort = "ultra"
	requireReject(t, badDefault, "", "", "INVALID_EFFORT")

	// Non-thinking entry only accepts none.
	requireReject(t, msc, "glm-4.7", "low", "INVALID_EFFORT")
	requireReject(t, msc, "glm-4.7", "high", "INVALID_EFFORT")

	// Effort-only on a non-thinking DEFAULT is likewise rejected.
	nonThinkingDefault := msc
	nonThinkingDefault.Default = "glm-4.7"
	requireReject(t, nonThinkingDefault, "", "low", "INVALID_EFFORT")
}

// TestResolveModelSelection_DeploymentDefaultClampedOnNonThinking: when the
// DEPLOYMENT default (not the request) lands on a non-thinking entry, the
// effort is clamped to none with a WARN and the turn proceeds — no 400 (the
// mismatch cannot be caught at load: the default entry may think while
// others do not).
func TestResolveModelSelection_DeploymentDefaultClampedOnNonThinking(t *testing.T) {
	msc := selectionTestCatalog()
	msc.DefaultEffort = "high"

	sel := requireResolve(t, msc, "glm-4.7", "")
	assert.Equal(t, "glm-4.7", sel.Model)
	assert.False(t, sel.Thinking)
	assert.Equal(t, "none", sel.Effort, "deployment default must clamp to none, not 400")
}

// TestResolveModelSelection_EmptyCatalogRejectsEverything: a hand-built
// selection config without a catalog cannot resolve any request (the
// mandatory-catalog guarantee; production cannot produce this shape because
// config load fails first).
func TestResolveModelSelection_EmptyCatalogRejectsEverything(t *testing.T) {
	msc := ModelSelectionConfig{}
	requireReject(t, msc, "", "", "INVALID_MODEL")
	requireReject(t, msc, "gpt-5", "", "INVALID_MODEL")
}

// TestResolveModelSelection_OutputSchemaConflict: a request resolving to
// effort != none conflicts with any configured agent output_schema and fails
// fast; effort = none is always compatible.
func TestResolveModelSelection_OutputSchemaConflict(t *testing.T) {
	msc := selectionTestCatalog()
	msc.AnyOutputSchema = true

	// Conflicting resolutions.
	requireReject(t, msc, "gpt-5", "high", "INVALID_EFFORT")
	requireReject(t, msc, "", "low", "INVALID_EFFORT") // effort-only, thinking default

	// A non-none deployment default conflicts even on a parameter-less turn.
	hi := selectionTestCatalog()
	hi.AnyOutputSchema = true
	hi.DefaultEffort = "medium"
	requireReject(t, hi, "", "", "INVALID_EFFORT")

	// effort=none stays compatible with structured output.
	sel := requireResolve(t, msc, "gpt-5", "none")
	assert.Equal(t, "none", sel.Effort)
}

// TestResolveModelSelection_WhitespaceTolerance: surrounding whitespace is
// trimmed before validation, so " gpt-5 " selects gpt-5.
func TestResolveModelSelection_WhitespaceTolerance(t *testing.T) {
	sel := requireResolve(t, selectionTestCatalog(), " gpt-5 ", " high ")
	assert.Equal(t, "gpt-5", sel.Model)
	assert.Equal(t, "high", sel.Effort)
}

// TestNewModelSelectionConfig verifies the derivation from a loaded config:
// the catalog, default entry, and normalized deployment default effort;
// AnyOutputSchema scans all three agents.
func TestNewModelSelectionConfig(t *testing.T) {
	t.Run("full derivation", func(t *testing.T) {
		cfg := &config.Config{
			OpenAI: config.OpenAIConfig{
				Models:                 []config.ModelCatalogEntry{{Name: "m-a", MaxContextTokens: 100000, Thinking: true}, {Name: "m-b", MaxContextTokens: 50000, Thinking: false}},
				DefaultModel:           "m-b",
				DefaultReasoningEffort: "high",
			},
			Agents: config.AgentsConfig{
				Confucius: config.AgentConfig{Name: "Confucius"},
				Chongzhi:  config.AgentConfig{Name: "Chongzhi"},
			},
		}
		msc := NewModelSelectionConfig(cfg)
		assert.Equal(t, "m-b", msc.Default)
		assert.Equal(t, "high", msc.DefaultEffort)
		assert.False(t, msc.AnyOutputSchema)
	})

	t.Run("empty effort normalizes to none", func(t *testing.T) {
		cfg := &config.Config{
			OpenAI: config.OpenAIConfig{
				Models: []config.ModelCatalogEntry{{Name: "m-a", MaxContextTokens: 100000}},
			},
			Agents: config.AgentsConfig{
				Liang: config.AgentConfig{Name: "Liang", OutputSchema: `{"type":"object"}`},
			},
		}
		msc := NewModelSelectionConfig(cfg)
		assert.Equal(t, "none", msc.DefaultEffort)
		assert.True(t, msc.AnyOutputSchema, "Liang's output_schema must set the flag")
	})

	t.Run("nil config", func(t *testing.T) {
		assert.Equal(t, ModelSelectionConfig{}, NewModelSelectionConfig(nil))
	})
}
