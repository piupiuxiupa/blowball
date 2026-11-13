package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDefaultAgentMaxRounds: the cap default is 100 (reproduces the prior
// hard-coded constants exactly; zero behavior change) (task 7.1).
func TestDefaultAgentMaxRounds(t *testing.T) {
	if got := DefaultAgentMaxRounds(); got != 100 {
		t.Fatalf("DefaultAgentMaxRounds() = %d, want 100", got)
	}
}

// TestAgentConfig_MaxRoundsUnsetIsZero: an unset max_rounds is the zero value;
// the default is applied at agent construction, not at config load (task 7.1).
func TestAgentConfig_MaxRoundsUnsetIsZero(t *testing.T) {
	var a AgentConfig
	if a.MaxRounds != 0 {
		t.Fatalf("unset MaxRounds = %d, want 0", a.MaxRounds)
	}
}

// TestAgentConfig_MaxRoundsParses: max_rounds parses from YAML (task 7.1).
func TestAgentConfig_MaxRoundsParses(t *testing.T) {
	var a AgentsConfig
	if err := yaml.Unmarshal([]byte("confucius:\n  max_rounds: 3\n"), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := a.Confucius.MaxRounds; got != 3 {
		t.Fatalf("parsed confucius max_rounds = %d, want 3", got)
	}
}

// TestAgentsConfigValidate_MaxRoundsNegative: a negative max_rounds is a typo,
// not "use default", and is rejected at load (task 7.1).
func TestAgentsConfigValidate_MaxRoundsNegative(t *testing.T) {
	a := AgentsConfig{Confucius: AgentConfig{MaxRounds: -1}}
	if err := a.validate(map[string]struct{}{}); err == nil {
		t.Fatal("expected error for negative max_rounds, got nil")
	}
}
