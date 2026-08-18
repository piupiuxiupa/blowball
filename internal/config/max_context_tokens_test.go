package config

import (
	"fmt"
	"strings"
	"testing"
)

// validBaseYAML is the minimal loadable config; the max_context_tokens cases
// below splice a value into the openai block via openaiExtra ("" for unset).
const validBaseYAML = `
openai:
  api_key: sk-test
  model: gpt-4o-mini
%s
mysql:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
jwt:
  secret: "super-secret"
  expire: 7d
agents:
  confucius:
    name: Confucius
    model: gpt-4o-mini
    system_prompt: "you are confucius"
`

func loadWithMaxContextTokens(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(validBaseYAML, extra)))
}

// TestLoad_MaxContextTokens_UnsetIsZeroDisabled covers the default: an omitted
// openai.max_context_tokens loads as 0, which the compaction service reads as
// "disabled" (context-compaction spec: zero value disables compaction).
func TestLoad_MaxContextTokens_UnsetIsZeroDisabled(t *testing.T) {
	cfg, err := loadWithMaxContextTokens(t, "")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.MaxContextTokens != 0 {
		t.Errorf("OpenAI.MaxContextTokens = %d, want 0 (unset disables compaction)", cfg.OpenAI.MaxContextTokens)
	}
}

// TestLoad_MaxContextTokens_ExplicitZero passes: an explicit zero is the same
// documented off switch as omitting the key.
func TestLoad_MaxContextTokens_ExplicitZero(t *testing.T) {
	cfg, err := loadWithMaxContextTokens(t, "  max_context_tokens: 0")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.MaxContextTokens != 0 {
		t.Errorf("OpenAI.MaxContextTokens = %d, want 0", cfg.OpenAI.MaxContextTokens)
	}
}

// TestLoad_MaxContextTokens_Positive passes a positive window through.
func TestLoad_MaxContextTokens_Positive(t *testing.T) {
	cfg, err := loadWithMaxContextTokens(t, "  max_context_tokens: 128000")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.MaxContextTokens != 128000 {
		t.Errorf("OpenAI.MaxContextTokens = %d, want 128000", cfg.OpenAI.MaxContextTokens)
	}
}

// TestLoad_MaxContextTokens_NegativeRejected: a negative value is necessarily
// a typo, and the spec's "Invalid value fails config load" scenario requires
// the server not to start.
func TestLoad_MaxContextTokens_NegativeRejected(t *testing.T) {
	_, err := loadWithMaxContextTokens(t, "  max_context_tokens: -5")
	if err == nil {
		t.Fatal("Load accepted a negative openai.max_context_tokens; want config validation error")
	}
	if !strings.Contains(err.Error(), "openai.max_context_tokens") {
		t.Errorf("error %q does not name openai.max_context_tokens", err)
	}
}

// TestLoad_MaxContextTokens_NonIntegerRejected: yaml.v3 silently truncates a
// fractional value into an int field (12.5 → 12), so the loader shadow-checks
// the raw value — a non-integer MUST fail load, not silently round.
func TestLoad_MaxContextTokens_NonIntegerRejected(t *testing.T) {
	// 12.5 is caught by the shadow check, which names the field.
	_, err := loadWithMaxContextTokens(t, "  max_context_tokens: 12.5")
	if err == nil {
		t.Fatal("Load accepted fractional openai.max_context_tokens 12.5; want an error")
	}
	if !strings.Contains(err.Error(), "openai.max_context_tokens") {
		t.Errorf("error %q does not name openai.max_context_tokens", err)
	}
	// A string value fails the main unmarshal itself (line-level error).
	if _, err := loadWithMaxContextTokens(t, `  max_context_tokens: "128k"`); err == nil {
		t.Fatal(`Load accepted string openai.max_context_tokens "128k"; want an error`)
	}
}
