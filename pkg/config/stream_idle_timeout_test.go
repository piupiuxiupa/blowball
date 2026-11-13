package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// loadWithStreamIdleTimeout splices an extra openai block line into the
// minimal loadable config (same harness as max_context_tokens_test.go; ""
// leaves the field unset).
func loadWithStreamIdleTimeout(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return Load(writeTempYAML(t, fmt.Sprintf(validBaseYAML, extra)))
}

// TestLoad_StreamIdleTimeout_UnsetIsZeroDisabled covers the default: an
// omitted openai.stream_idle_timeout loads as 0, which the LLM client reads
// as "watchdog disabled" (llm-stream-watchdog spec: zero/unset keeps prior
// behavior byte-for-byte).
func TestLoad_StreamIdleTimeout_UnsetIsZeroDisabled(t *testing.T) {
	cfg, err := loadWithStreamIdleTimeout(t, "")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.StreamIdleTimeout != 0 {
		t.Errorf("OpenAI.StreamIdleTimeout = %s, want 0 (unset disables the watchdog)", cfg.OpenAI.StreamIdleTimeout)
	}
}

// TestLoad_StreamIdleTimeout_ExplicitZero passes: an explicit zero is the same
// documented off switch as omitting the key.
func TestLoad_StreamIdleTimeout_ExplicitZero(t *testing.T) {
	cfg, err := loadWithStreamIdleTimeout(t, "  stream_idle_timeout: 0s")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.StreamIdleTimeout != 0 {
		t.Errorf("OpenAI.StreamIdleTimeout = %s, want 0", cfg.OpenAI.StreamIdleTimeout)
	}
}

// TestLoad_StreamIdleTimeout_Positive passes the recommended value through
// with duration-suffix parsing.
func TestLoad_StreamIdleTimeout_Positive(t *testing.T) {
	cfg, err := loadWithStreamIdleTimeout(t, "  stream_idle_timeout: 2m")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.StreamIdleTimeout != 2*time.Minute {
		t.Errorf("OpenAI.StreamIdleTimeout = %s, want 2m0s", cfg.OpenAI.StreamIdleTimeout)
	}
}

// TestLoad_StreamIdleTimeout_NegativeRejected: a negative duration is
// necessarily a typo — zero is the documented off switch — and the spec's
// "Negative value fails config load" scenario requires the server not to
// start.
func TestLoad_StreamIdleTimeout_NegativeRejected(t *testing.T) {
	_, err := loadWithStreamIdleTimeout(t, "  stream_idle_timeout: -1s")
	if err == nil {
		t.Fatal("Load accepted a negative openai.stream_idle_timeout; want config validation error")
	}
	if !strings.Contains(err.Error(), "openai.stream_idle_timeout") {
		t.Errorf("error %q does not name openai.stream_idle_timeout", err)
	}
}

// TestLoad_StreamIdleTimeout_NoShadowCheckNeeded documents why the
// max_context_tokens-style raw-value shadow check is unnecessary here:
// yaml.v3 decodes durations through time.ParseDuration, which parses
// fractional values exactly (1.5s → 1500ms — no silent truncation) and
// rejects non-duration garbage at unmarshal time.
func TestLoad_StreamIdleTimeout_NoShadowCheckNeeded(t *testing.T) {
	cfg, err := loadWithStreamIdleTimeout(t, "  stream_idle_timeout: 1.5s")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.OpenAI.StreamIdleTimeout != 1500*time.Millisecond {
		t.Errorf("OpenAI.StreamIdleTimeout = %s, want 1.5s exactly", cfg.OpenAI.StreamIdleTimeout)
	}
	if _, err := loadWithStreamIdleTimeout(t, "  stream_idle_timeout: banana"); err == nil {
		t.Fatal(`Load accepted non-duration openai.stream_idle_timeout "banana"; want an unmarshal error`)
	}
}
