package executor

import (
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

func TestRegisterAllSkipsWhenUnavailable(t *testing.T) {
	if available {
		t.Skip("bwrap is available on this platform")
	}

	reg := tool.NewRegistry()
	tools := NewTools(config.ExecutorConfig{
		Bash:   config.ExecutorToolConfig{Enabled: true},
		Python: config.ExecutorToolConfig{Enabled: true},
	}, func(string) string { return "/workspace" }, func(string) string { return "/skills/user" }, "/skills/global", "/tools")

	if err := RegisterAll(reg, tools); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := reg.Get(ToolBash); ok {
		t.Error("expected bash tool to be absent")
	}
	if _, ok := reg.Get(ToolPython); ok {
		t.Error("expected python tool to be absent")
	}
}

func TestRegisterAllDisabled(t *testing.T) {
	reg := tool.NewRegistry()
	tools := NewTools(config.ExecutorConfig{
		Bash:   config.ExecutorToolConfig{Enabled: false},
		Python: config.ExecutorToolConfig{Enabled: false},
	}, func(string) string { return "/workspace" }, func(string) string { return "/skills/user" }, "/skills/global", "/tools")

	if err := RegisterAll(reg, tools); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := reg.Get(ToolBash); ok {
		t.Error("expected bash tool to be absent when disabled")
	}
	if _, ok := reg.Get(ToolPython); ok {
		t.Error("expected python tool to be absent when disabled")
	}
}

func TestNewToolsResolvers(t *testing.T) {
	tools := NewTools(config.ExecutorConfig{}, func(userID string) string {
		return "/data/" + userID + "/workspace"
	}, func(userID string) string {
		return "/data/" + userID + "/skills"
	}, "/app/skills", "/app/tools")
	if tools.workspaceFn("u1") != "/data/u1/workspace" {
		t.Errorf("unexpected workspace path: %q", tools.workspaceFn("u1"))
	}
	if tools.userSkillsFn("u1") != "/data/u1/skills" {
		t.Errorf("unexpected user skills path: %q", tools.userSkillsFn("u1"))
	}
	if tools.globalSkillsDir != "/app/skills" {
		t.Errorf("unexpected global skills path: %q", tools.globalSkillsDir)
	}
	if tools.toolsDir != "/app/tools" {
		t.Errorf("unexpected tools path: %q", tools.toolsDir)
	}
}
