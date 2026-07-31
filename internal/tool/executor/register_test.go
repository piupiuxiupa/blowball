package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

func TestBuildPipCommand(t *testing.T) {
	cfg := config.PipToolConfig{
		IndexURL:       "https://pypi.tuna.tsinghua.edu.cn/simple",
		ExtraIndexURLs: []string{"https://extra.example.com/simple"},
		TrustedHosts:   []string{"pypi.tuna.tsinghua.edu.cn", "extra.example.com"},
	}
	args := pipArgs{Packages: []string{"requests", "numpy>=2.0"}, Upgrade: true}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	want := []string{
		"python3", "-m", "pip", "install", "--target", "/workspace/.pip",
		"--upgrade",
		"-i", "https://pypi.tuna.tsinghua.edu.cn/simple",
		"--extra-index-url", "https://extra.example.com/simple",
		"--trusted-host", "pypi.tuna.tsinghua.edu.cn",
		"--trusted-host", "extra.example.com",
		"requests", "numpy>=2.0",
	}
	if len(cmd) != len(want) {
		t.Fatalf("command length mismatch: got %d, want %d\ngot: %v", len(cmd), len(want), cmd)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Errorf("cmd[%d] = %q, want %q", i, cmd[i], want[i])
		}
	}
}

func TestBuildPipCommandMinimal(t *testing.T) {
	cfg := config.PipToolConfig{}
	args := pipArgs{Packages: []string{"colorama"}}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	want := []string{"python3", "-m", "pip", "install", "--target", "/workspace/.pip", "colorama"}
	if len(cmd) != len(want) {
		t.Fatalf("command length mismatch: got %d, want %d\ngot: %v", len(cmd), len(want), cmd)
	}
	for i := range want {
		if cmd[i] != want[i] {
			t.Errorf("cmd[%d] = %q, want %q", i, cmd[i], want[i])
		}
	}
}

func TestBuildPipCommandIgnoresEmptyOptions(t *testing.T) {
	cfg := config.PipToolConfig{
		IndexURL:       "",
		ExtraIndexURLs: []string{"", "https://valid.example.com/simple", ""},
		TrustedHosts:   []string{"", "valid.example.com", ""},
	}
	args := pipArgs{Packages: []string{"requests"}}

	cmd, err := buildPipCommand(cfg, args)
	if err != nil {
		t.Fatalf("buildPipCommand returned error: %v", err)
	}

	for i, v := range cmd {
		if v == "" {
			t.Errorf("command contains empty argument at index %d: %v", i, cmd)
		}
	}
	if slices.Contains(cmd, "-i") {
		t.Error("expected no -i flag for empty index_url")
	}
}

// TestRegister_DescriptionDeclaresResultShapeAndAntiPattern pins the executor
// output contract ({output, exit_code, truncated}, 64KB cap), the xizhi_* file-
// tool anti-pattern for bash/python, and the python code/file mutual exclusion
// (delta spec executor-tools).
func TestRegister_DescriptionDeclaresResultShapeAndAntiPattern(t *testing.T) {
	r := tool.NewRegistry()
	tools := NewTools(config.ExecutorConfig{}, func(string) string { return "" }, "", "")
	require.NoError(t, registerBash(r, tools))
	require.NoError(t, registerPython(r, tools))
	require.NoError(t, registerPip(r, tools))

	for _, name := range []string{ToolBash, ToolPython, ToolPip} {
		spec, ok := r.Get(name)
		require.True(t, ok, name)
		assert.Contains(t, spec.Description, "output", name)
		assert.Contains(t, spec.Description, "exit_code", name)
		assert.Contains(t, spec.Description, "truncated", name)
		assert.Contains(t, spec.Description, "64KB", name)
	}

	bash, ok := r.Get(ToolBash)
	require.True(t, ok)
	assert.Contains(t, bash.Description, "xizhi")

	python, ok := r.Get(ToolPython)
	require.True(t, ok)
	assert.Contains(t, python.Description, "xizhi")
	// code/file mutual exclusion.
	assert.Contains(t, python.Description, "code")
	assert.Contains(t, python.Description, "file")
	assert.Contains(t, python.Description, "Exactly one")

	pip, ok := r.Get(ToolPip)
	require.True(t, ok)
	// Purpose boundary: install deps, not run code.
	assert.Contains(t, pip.Description, "ModuleNotFoundError")

	// R4: each executor description marks >=2 critical constraints with an
	// emphasized strong-imperative keyword (bold + UPPERCASE in the source).
	for _, name := range []string{ToolBash, ToolPython, ToolPip} {
		spec, ok := r.Get(name)
		require.True(t, ok, name)
		assert.GreaterOrEqual(t, countStrongImperatives(spec.Description), 2, name)
	}
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
