package config

import (
	"fmt"
	"strings"
	"testing"
)

// validBaseYAML is the minimal loadable config under model-effort-v2: a
// mandatory one-entry openai.models catalog and no model fields on the agent.
// The residual-field cases below splice removed keys into the openai block
// via openaiExtra ("" for a clean load).
const validBaseYAML = `
openai:
  api_key: sk-test
  models:
    - name: gpt-4o-mini
      max_context_tokens: 128000
      max_completion_tokens: 8192
%s
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "super-secret"
  expire: 7d
agents:
  confucius:
    name: Confucius
    system_prompt: "you are confucius"
`

// loadWithRemovedOpenAIField loads validBaseYAML with an extra raw line
// spliced into the openai block.
func loadWithRemovedOpenAIField(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(validBaseYAML, extra)))
}

// TestLoad_OpenAIModelResidualRejected: openai.model was renamed
// openai.title_model by model-effort-v2; a residual key fails load with an
// error pointing at the migration (any value, including a null/empty one,
// counts as residual — the typed decode would silently drop it).
func TestLoad_OpenAIModelResidualRejected(t *testing.T) {
	for _, extra := range []string{
		"  model: gpt-4o-mini\n",
		"  model: \"\"\n",
		"  model:\n",
	} {
		_, err := loadWithRemovedOpenAIField(t, extra)
		if err == nil {
			t.Fatalf("Load accepted residual openai.model %q; want migration error", extra)
		}
		if !strings.Contains(err.Error(), "openai.model was removed") || !strings.Contains(err.Error(), "openai.title_model") {
			t.Errorf("error %q does not point at the openai.title_model migration", err)
		}
	}
}

// TestLoad_OpenAIMaxContextTokensResidualRejected: the top-level
// openai.max_context_tokens moved into the catalog entries; every residual
// form (zero, negative, fractional, string) fails load pointing at the
// replacement — never silently ignored, never rounded.
func TestLoad_OpenAIMaxContextTokensResidualRejected(t *testing.T) {
	for _, extra := range []string{
		"  max_context_tokens: 0\n",
		"  max_context_tokens: 128000\n",
		"  max_context_tokens: -5\n",
		"  max_context_tokens: 12.5\n",
		"  max_context_tokens: \"128k\"\n",
		"  max_context_tokens:\n",
	} {
		_, err := loadWithRemovedOpenAIField(t, extra)
		if err == nil {
			t.Fatalf("Load accepted residual openai.max_context_tokens %q; want migration error", extra)
		}
		if !strings.Contains(err.Error(), "openai.max_context_tokens was removed") || !strings.Contains(err.Error(), "openai.models") {
			t.Errorf("error %q does not point at the openai.models catalog migration", err)
		}
	}
}

// TestLoad_BaseConfigLoads: the new minimal base config loads cleanly with
// the catalog required (regression guard for the harness itself).
func TestLoad_BaseConfigLoads(t *testing.T) {
	cfg, err := loadWithRemovedOpenAIField(t, "")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.DefaultModelName(); got != "gpt-4o-mini" {
		t.Errorf("DefaultModelName() = %q, want gpt-4o-mini", got)
	}
	if cfg.OpenAI.DefaultReasoningEffort != "none" {
		t.Errorf("DefaultReasoningEffort = %q, want none (normalized default)", cfg.OpenAI.DefaultReasoningEffort)
	}
}
