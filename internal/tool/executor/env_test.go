package executor

import (
	"os"
	"testing"
)

func TestFilterEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/alice")
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("PYTHONPATH", "/usr/lib/python")
	t.Setenv("PYTHONHOME", "/usr")

	got := filterEnv([]string{"PATH", "HOME", "PYTHON*"})

	if got["PATH"] != "/usr/bin" {
		t.Errorf("expected PATH to be preserved, got %q", got["PATH"])
	}
	if got["HOME"] != "/home/alice" {
		t.Errorf("expected HOME to be preserved, got %q", got["HOME"])
	}
	if _, ok := got["OPENAI_API_KEY"]; ok {
		t.Error("OPENAI_API_KEY should not be preserved")
	}
	if got["PYTHONPATH"] != "/usr/lib/python" {
		t.Errorf("expected PYTHONPATH to be preserved, got %q", got["PYTHONPATH"])
	}
	if got["PYTHONHOME"] != "/usr" {
		t.Errorf("expected PYTHONHOME to be preserved, got %q", got["PYTHONHOME"])
	}
}

func TestFilterEnvEmptyPatterns(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	got := filterEnv(nil)
	if len(got) != 0 {
		t.Errorf("expected no env vars, got %v", got)
	}
}

func TestMatchesAny(t *testing.T) {
	patterns := []string{"PATH", "PYTHON*", "TERM"}
	if !matchesAny("PATH", patterns) {
		t.Error("expected PATH to match")
	}
	if !matchesAny("PYTHONPATH", patterns) {
		t.Error("expected PYTHONPATH to match PYTHON*")
	}
	if matchesAny("OPENAI_API_KEY", patterns) {
		t.Error("expected OPENAI_API_KEY not to match")
	}
}

func TestFilterEnvDoesNotMutateProcess(t *testing.T) {
	t.Setenv("DEMO_VAR", "demo-value")
	_ = filterEnv([]string{"DEMO_VAR"})
	if os.Getenv("DEMO_VAR") != "demo-value" {
		t.Error("filterEnv should not modify the host environment")
	}
}
