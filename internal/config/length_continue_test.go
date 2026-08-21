package config

import (
	"fmt"
	"strings"
	"testing"
)

// loadWithEntryLengthContinue splices extra openai.models[] ENTRY lines into
// the minimal loadable config (per-model-completion-budget: the
// length_continue block moved from the global openai level into each catalog
// entry, so the harness splices at entry level — 6-space indent).
func loadWithEntryLengthContinue(t *testing.T, entryExtra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(`
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
`, entryExtra)))
}

// TestLoad_LengthContinue_UnsetDisabled covers the default: an entry without
// a length_continue sub-block loads with continuation disabled
// (llm-length-continuation spec: zero/unset keeps prior behavior
// byte-for-byte for that entry).
func TestLoad_LengthContinue_UnsetDisabled(t *testing.T) {
	cfg, err := loadWithEntryLengthContinue(t, "")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	lc := cfg.OpenAI.Models[0].LengthContinue
	if lc.Enabled() {
		t.Errorf("LengthContinue.Enabled() = true, want false (unset disables the capability for the entry)")
	}
	if step, retries := lc.Resolve(); step != 0 || retries != 0 {
		t.Errorf("LengthContinue.Resolve() = (%d, %d), want (0, 0) when disabled", step, retries)
	}
}

// TestLoad_LengthContinue_ExplicitZeroBlockDisabled: an all-zero sub-block is
// the same documented off switch as omitting it.
func TestLoad_LengthContinue_ExplicitZeroBlockDisabled(t *testing.T) {
	cfg, err := loadWithEntryLengthContinue(t, "      length_continue:\n        expand_step: 0\n        max_retries: 0")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.Models[0].LengthContinue.Enabled() {
		t.Errorf("LengthContinue.Enabled() = true, want false for an all-zero block")
	}
}

// TestLoad_LengthContinue_PartialConfigDefaultsSibling: any non-zero field
// enables the capability for the entry and the missing sibling resolves to
// its default.
func TestLoad_LengthContinue_PartialConfigDefaultsSibling(t *testing.T) {
	cfg, err := loadWithEntryLengthContinue(t, "      length_continue:\n        expand_step: 4096")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	lc := cfg.OpenAI.Models[0].LengthContinue
	if !lc.Enabled() {
		t.Fatalf("LengthContinue.Enabled() = false, want true for expand_step: 4096")
	}
	step, retries := lc.Resolve()
	if step != 4096 {
		t.Errorf("Resolve() expandStep = %d, want 4096 (explicit)", step)
	}
	if retries != DefaultLengthContinueRetries() {
		t.Errorf("Resolve() maxRetries = %d, want default %d", retries, DefaultLengthContinueRetries())
	}

	cfg, err = loadWithEntryLengthContinue(t, "      length_continue:\n        max_retries: 5")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	step, retries = cfg.OpenAI.Models[0].LengthContinue.Resolve()
	if step != DefaultLengthContinueStep() {
		t.Errorf("Resolve() expandStep = %d, want default %d", step, DefaultLengthContinueStep())
	}
	if retries != 5 {
		t.Errorf("Resolve() maxRetries = %d, want 5 (explicit)", retries)
	}
}

// TestLoad_LengthContinue_FullBlock passes explicit values through.
func TestLoad_LengthContinue_FullBlock(t *testing.T) {
	cfg, err := loadWithEntryLengthContinue(t, "      length_continue:\n        expand_step: 8192\n        max_retries: 3")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	step, retries := cfg.OpenAI.Models[0].LengthContinue.Resolve()
	if step != 8192 || retries != 3 {
		t.Errorf("Resolve() = (%d, %d), want (8192, 3)", step, retries)
	}
}

// TestLoad_LengthContinue_NegativeRejected: negatives are typos — zero is the
// documented off switch — and fail config load.
func TestLoad_LengthContinue_NegativeRejected(t *testing.T) {
	for _, extra := range []string{
		"      length_continue:\n        expand_step: -1",
		"      length_continue:\n        max_retries: -3",
	} {
		if _, err := loadWithEntryLengthContinue(t, extra); err == nil {
			t.Errorf("Load succeeded for %q, want a validation error", extra)
		}
	}
}

// TestLoad_LengthContinue_PerEntryNoBleed: two catalog entries with different
// length_continue configurations never bleed into each other — the activation
// and both parameters are per-entry (per-model-completion-budget D3).
func TestLoad_LengthContinue_PerEntryNoBleed(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - name: model-a
      max_context_tokens: 128000
      max_completion_tokens: 8192
      length_continue:
        expand_step: 4096
        max_retries: 1
    - name: model-b
      max_context_tokens: 64000
      max_completion_tokens: 4096
      length_continue:
        expand_step: 16384
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
	if len(cfg.OpenAI.Models) != 2 {
		t.Fatalf("catalog len = %d, want 2", len(cfg.OpenAI.Models))
	}
	a, b := cfg.OpenAI.Models[0].LengthContinue, cfg.OpenAI.Models[1].LengthContinue
	if !a.Enabled() || !b.Enabled() {
		t.Fatalf("both entries should be enabled, got a=%v b=%v", a.Enabled(), b.Enabled())
	}
	if step, retries := a.Resolve(); step != 4096 || retries != 1 {
		t.Errorf("model-a Resolve() = (%d, %d), want (4096, 1)", step, retries)
	}
	// model-b omits max_retries → default 3, and its expand_step is its own.
	if step, retries := b.Resolve(); step != 16384 || retries != DefaultLengthContinueRetries() {
		t.Errorf("model-b Resolve() = (%d, %d), want (16384, %d)", step, retries, DefaultLengthContinueRetries())
	}

	// A third entry with no block stays disabled alongside configured
	// siblings — configuration on one entry never turns the feature on for
	// the whole deployment.
	cfg, err = loadWithEntryLengthContinue(t, "  # second entry below\n    - name: model-c\n      max_context_tokens: 32000\n      max_completion_tokens: 2048")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.Models[0].LengthContinue.Enabled() {
		t.Errorf("model without length_continue loaded enabled; want disabled")
	}
}

// TestLoad_LengthContinue_GlobalBlockResidualRejected: the global
// openai.length_continue block was moved into catalog entries by
// per-model-completion-budget; a residual global block fails load with an
// error pointing at the migration (any value, including a null one, counts —
// the typed decode would silently drop it).
func TestLoad_LengthContinue_GlobalBlockResidualRejected(t *testing.T) {
	for _, extra := range []string{
		"  length_continue:\n    expand_step: 8192\n",
		"  length_continue:\n",
		"  length_continue: {}\n",
	} {
		_, err := loadWithRemovedOpenAIField(t, extra)
		if err == nil {
			t.Fatalf("Load accepted residual openai.length_continue %q; want migration error", extra)
		}
		if !strings.Contains(err.Error(), "openai.length_continue was removed") || !strings.Contains(err.Error(), "openai.models") {
			t.Errorf("error %q does not point at the openai.models[].length_continue migration", err)
		}
	}
}
