package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

// TestRegister_DescriptionDeclaresResultShapeAndAntiPattern pins the bash output
// contract ({output, exit_code, truncated}, 64KB cap, 30s timeout) and the
// expanded xizhi_* file-tool anti-pattern (delta spec executor-tools: bash
// description declares result shape, limits, and file-tool anti-pattern).
func TestRegister_DescriptionDeclaresResultShapeAndAntiPattern(t *testing.T) {
	r := tool.NewRegistry()
	tools := NewTools(config.ExecutorConfig{}, func(string) string { return "" }, "", "")
	require.NoError(t, registerBash(r, tools))

	spec, ok := r.Get(ToolBash)
	require.True(t, ok)
	// Result shape.
	assert.Contains(t, spec.Description, "output")
	assert.Contains(t, spec.Description, "exit_code")
	assert.Contains(t, spec.Description, "truncated")
	// Output cap + truncation marker.
	assert.Contains(t, spec.Description, "64KB")
	// Timeout.
	assert.Contains(t, spec.Description, "30s")
	// Expanded file-tool anti-pattern: each keyword maps to a dedicated tool.
	for _, kw := range []string{"cat", "rm", "ls", "find", "sed", "awk", "grep"} {
		assert.Contains(t, spec.Description, kw, "anti-pattern should mention %q", kw)
	}
	assert.Contains(t, spec.Description, "xizhi_read_file")
	assert.Contains(t, spec.Description, "xizhi_list_files")
	assert.Contains(t, spec.Description, "xizhi_tree")
	assert.Contains(t, spec.Description, "xizhi_glob_files")
	assert.Contains(t, spec.Description, "xizhi_grep")
	assert.Contains(t, spec.Description, "xizhi_modify_file")
	assert.Contains(t, spec.Description, "xizhi_delete")
	// pip-via-bash + PYTHONPATH bridge guidance is present.
	assert.Contains(t, spec.Description, "python3 -m pip install")
	assert.Contains(t, spec.Description, "PYTHONPATH")
	// DO NOT strong imperative present.
	assert.Contains(t, spec.Description, "DO NOT")

	// R4: the description marks >=2 critical constraints with an emphasized
	// strong-imperative keyword (bold + UPPERCASE in the source).
	assert.GreaterOrEqual(t, countStrongImperatives(spec.Description), 2)
}

// countStrongImperatives counts emphasized imperative keywords (MUST / DO NOT /
// MUST NOT / IMPORTANT / REQUIRED / ONLY / SHOULD / NEVER) in a description.
func countStrongImperatives(desc string) int {
	n := 0
	for _, kw := range []string{"MUST NOT", "MUST", "DO NOT", "IMPORTANT", "REQUIRED", "ONLY", "SHOULD", "NEVER"} {
		n += strings.Count(desc, kw)
	}
	return n
}
