package executor

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// filterEnv returns the subset of the current process environment whose keys
// match at least one pattern in allowedPatterns. Patterns use filepath.Match
// syntax, so "PYTHON*" matches PYTHONPATH, PYTHONHOME, etc.
func filterEnv(allowedPatterns []string) map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if matchesAny(k, allowedPatterns) {
			out[k] = v
		}
	}
	return out
}

// matchesAny reports whether s matches any of the glob patterns.
func matchesAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, s); matched {
			return true
		}
	}
	return false
}

// mergeEnv applies the operator env-literal layer (design D2): every entry in
// overrides is written into dst, overriding any same-named host allowlist
// variable already present. It runs AFTER filterEnv (host layer) and BEFORE the
// forced-invariant layer (HOME/PATH/PYTHONPATH), so the forced layer always
// applies last and always wins. dst is the env map being constructed and is
// mutated in place; overrides (the operator config) is only read, never mutated.
func mergeEnv(dst, overrides map[string]string) {
	maps.Copy(dst, overrides)
}
