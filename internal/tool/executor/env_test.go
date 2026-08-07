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

// TestMergeEnv pins the operator env-literal layer (design D2): overrides are
// written into dst, overriding same-named host allowlist variables, while the
// overrides map itself (the operator config) is not mutated.
func TestMergeEnv(t *testing.T) {
	dst := map[string]string{"FOO": "host", "BAR": "keep"}
	overrides := map[string]string{"FOO": "cfg", "BAZ": "new"}

	mergeEnv(dst, overrides)

	wantDst := map[string]string{"FOO": "cfg", "BAR": "keep", "BAZ": "new"}
	if !equalMap(dst, wantDst) {
		t.Errorf("dst = %v, want %v (override semantics)", dst, wantDst)
	}

	// The overrides (operator config) must not be polluted.
	wantOverrides := map[string]string{"FOO": "cfg", "BAZ": "new"}
	if !equalMap(overrides, wantOverrides) {
		t.Errorf("overrides mutated to %v, want %v (input not polluted)", overrides, wantOverrides)
	}
}

// TestMergeEnvEmptyOverridesIsNoop pins the zero-behavior-change guarantee: an
// unset or empty overrides map leaves dst untouched.
func TestMergeEnvEmptyOverridesIsNoop(t *testing.T) {
	dst := map[string]string{"FOO": "host"}
	before := map[string]string{"FOO": "host"}

	mergeEnv(dst, nil)
	if !equalMap(dst, before) {
		t.Errorf("dst = %v after mergeEnv(nil), want unchanged %v", dst, before)
	}

	mergeEnv(dst, map[string]string{})
	if !equalMap(dst, before) {
		t.Errorf("dst = %v after mergeEnv({}), want unchanged %v", dst, before)
	}
}

// equalMap is a small value equality check for two string maps (test-only; the
// standard library has no generic map equality on this Go line for our use).
func equalMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
