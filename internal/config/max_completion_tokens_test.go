package config

import (
	"fmt"
	"strings"
	"testing"
)

// loadWithQuota loads the minimal config with the gpt-4o-mini entry's
// max_completion_tokens replaced by the given raw YAML fragment (the whole
// `max_completion_tokens: ...` line, or "" to omit the field).
func loadWithQuota(t *testing.T, rawQuota string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(`
openai:
  api_key: sk-test
  models:
    - name: gpt-4o-mini
      max_context_tokens: 128000
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
`, rawQuota)))
}

// TestLoad_MaxCompletionTokens_RequiredPositiveInteger: the entry quota is
// mandatory with the same validation strength as max_context_tokens —
// missing, zero, negative, fractional (yaml truncation shadow-check) and
// non-numeric values all fail load, the error naming the entry.
func TestLoad_MaxCompletionTokens_RequiredPositiveInteger(t *testing.T) {
	for _, raw := range []string{
		"",                                    // missing → rejected
		"      max_completion_tokens: 0\n",    // zero
		"      max_completion_tokens: -5\n",   // negative
		"      max_completion_tokens: 8192.5", // fraction (yaml.v3 would truncate to 8192)
		"      max_completion_tokens: \"8k\"", // non-numeric (typed decode fails)
	} {
		_, err := loadWithQuota(t, raw)
		if err == nil {
			t.Fatalf("Load accepted max_completion_tokens %q; want a validation error", raw)
		}
		// The fraction is caught by the raw-value shadow check and the
		// missing/zero/negative values by validate(); the quoted string dies
		// earlier at the typed yaml decode, whose message names only the
		// line — so the field-named assertion covers every case the
		// quota validation itself owns.
		if strings.Contains(raw, "\"") {
			continue
		}
		if !strings.Contains(err.Error(), "max_completion_tokens") {
			t.Errorf("error %q does not mention max_completion_tokens", err)
		}
	}

	// The negative / missing variants come from the typed validate() path,
	// which names the offending entry (spec: the error names the entry and
	// the required field).
	for _, raw := range []string{"", "      max_completion_tokens: 0\n", "      max_completion_tokens: -5\n"} {
		_, err := loadWithQuota(t, raw)
		if err == nil || !strings.Contains(err.Error(), "gpt-4o-mini") {
			t.Errorf("error %v for %q does not name the entry gpt-4o-mini", err, raw)
		}
	}
}

// TestLoad_MaxCompletionTokens_LoadedPerEntry: a valid quota decodes onto the
// entry and differs per entry (per-model-completion-budget D1).
func TestLoad_MaxCompletionTokens_LoadedPerEntry(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - name: model-a
      max_context_tokens: 128000
      max_completion_tokens: 8192
    - name: model-b
      max_context_tokens: 64000
      max_completion_tokens: 4096
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "super-secret"
  expire: 7d
agents:
  confucius:
    name: Confucius
    system_prompt: "you are confucius"
`))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.Models[0].MaxCompletionTokens != 8192 {
		t.Errorf("model-a MaxCompletionTokens = %d, want 8192", cfg.OpenAI.Models[0].MaxCompletionTokens)
	}
	if cfg.OpenAI.Models[1].MaxCompletionTokens != 4096 {
		t.Errorf("model-b MaxCompletionTokens = %d, want 4096", cfg.OpenAI.Models[1].MaxCompletionTokens)
	}
}

// TestLoad_AgentMaxTokensResidualRejected: agents.<name>.max_tokens was
// removed by per-model-completion-budget (the quota moved to the catalog
// entries); a residual key fails load with an error pointing at the
// migration.
func TestLoad_AgentMaxTokensResidualRejected(t *testing.T) {
	agentBlock := func(name, line string) string {
		return fmt.Sprintf(`
agents:
  %s:
    name: %s
    system_prompt: "you are %s"
    %s
`, name, name, name, line)
	}
	// Splice an agents block carrying the residual max_tokens into the
	// minimal config (replacing the default clean one).
	loadWithAgentResidual := func(agentKey, line string) error {
		_, err := Load(writeTempYAML(t, fmt.Sprintf(`
openai:
  api_key: sk-test
  models:
    - name: gpt-4o-mini
      max_context_tokens: 128000
      max_completion_tokens: 8192
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "super-secret"
  expire: 7d
%s`, agentBlock(agentKey, line))))
		return err
	}
	for _, agent := range []string{"confucius", "chongzhi", "liang"} {
		for _, line := range []string{"max_tokens: 8192", "max_tokens: 0", "max_tokens:"} {
			err := loadWithAgentResidual(agent, line)
			if err == nil {
				t.Fatalf("Load accepted residual agents.%s.%s; want migration error", agent, line)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("agents.%s.max_tokens was removed", agent)) || !strings.Contains(err.Error(), "max_completion_tokens") {
				t.Errorf("error %q does not point at the openai.models[].max_completion_tokens migration", err)
			}
		}
	}
}
