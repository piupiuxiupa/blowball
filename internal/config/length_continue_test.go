package config

import (
	"fmt"
	"testing"
)

// loadWithLengthContinue splices extra openai block lines into the minimal
// loadable config (same harness as stream_idle_timeout_test.go).
func loadWithLengthContinue(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(validBaseYAML, extra)))
}

// TestLoad_LengthContinue_UnsetDisabled covers the default: an omitted
// openai.length_continue block loads disabled (llm-length-continuation spec:
// zero/unset keeps prior behavior byte-for-byte).
func TestLoad_LengthContinue_UnsetDisabled(t *testing.T) {
	cfg, err := loadWithLengthContinue(t, "")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.LengthContinue.Enabled() {
		t.Errorf("LengthContinue.Enabled() = true, want false (unset disables the capability)")
	}
	if step, retries := cfg.OpenAI.LengthContinue.Resolve(); step != 0 || retries != 0 {
		t.Errorf("LengthContinue.Resolve() = (%d, %d), want (0, 0) when disabled", step, retries)
	}
}

// TestLoad_LengthContinue_ExplicitZeroBlockDisabled: an all-zero block is the
// same documented off switch as omitting the block.
func TestLoad_LengthContinue_ExplicitZeroBlockDisabled(t *testing.T) {
	cfg, err := loadWithLengthContinue(t, "  length_continue:\n    expand_step: 0\n    max_retries: 0")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.LengthContinue.Enabled() {
		t.Errorf("LengthContinue.Enabled() = true, want false for an all-zero block")
	}
}

// TestLoad_LengthContinue_PartialConfigDefaultsSibling: any non-zero field
// enables the capability and the missing sibling resolves to its default
// (design D9).
func TestLoad_LengthContinue_PartialConfigDefaultsSibling(t *testing.T) {
	cfg, err := loadWithLengthContinue(t, "  length_continue:\n    expand_step: 4096")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.OpenAI.LengthContinue.Enabled() {
		t.Fatalf("LengthContinue.Enabled() = false, want true for expand_step: 4096")
	}
	step, retries := cfg.OpenAI.LengthContinue.Resolve()
	if step != 4096 {
		t.Errorf("Resolve() expandStep = %d, want 4096 (explicit)", step)
	}
	if retries != DefaultLengthContinueRetries() {
		t.Errorf("Resolve() maxRetries = %d, want default %d", retries, DefaultLengthContinueRetries())
	}

	cfg, err = loadWithLengthContinue(t, "  length_continue:\n    max_retries: 5")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	step, retries = cfg.OpenAI.LengthContinue.Resolve()
	if step != DefaultLengthContinueStep() {
		t.Errorf("Resolve() expandStep = %d, want default %d", step, DefaultLengthContinueStep())
	}
	if retries != 5 {
		t.Errorf("Resolve() maxRetries = %d, want 5 (explicit)", retries)
	}
}

// TestLoad_LengthContinue_FullBlock passes explicit values through.
func TestLoad_LengthContinue_FullBlock(t *testing.T) {
	cfg, err := loadWithLengthContinue(t, "  length_continue:\n    expand_step: 8192\n    max_retries: 3")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	step, retries := cfg.OpenAI.LengthContinue.Resolve()
	if step != 8192 || retries != 3 {
		t.Errorf("Resolve() = (%d, %d), want (8192, 3)", step, retries)
	}
}

// TestLoad_LengthContinue_NegativeRejected: negatives are typos — zero is the
// documented off switch — and fail config load.
func TestLoad_LengthContinue_NegativeRejected(t *testing.T) {
	for _, extra := range []string{
		"  length_continue:\n    expand_step: -1",
		"  length_continue:\n    max_retries: -3",
	} {
		if _, err := loadWithLengthContinue(t, extra); err == nil {
			t.Errorf("Load succeeded for %q, want a validation error", extra)
		}
	}
}
