package executor

import (
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
