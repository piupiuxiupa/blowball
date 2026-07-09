package executor

import (
	"slices"
	"testing"

	"github.com/lush/blowball/internal/config"
)

func TestBuildBwrapArgsIncludesRequiredFlags(t *testing.T) {
	cfg := config.ExecutorToolConfig{
		Network:            false,
		AllowedEnvPatterns: []string{"PATH"},
	}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	required := []string{
		"--unshare-user",
		"--unshare-ipc",
		"--unshare-pid",
		"--unshare-uts",
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", "/data/u1/workspace/tmp", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/etc", "/etc",
		"--bind", "/data/u1/workspace", "/workspace",
		"--ro-bind", "/skills/global", "/skills/global",
		"--ro-bind", "/data/u1/skills", "/skills/user",
		"--chdir", "/workspace",
		"--unshare-net",
		"--clearenv",
		"--setenv", "PATH",
	}

	for _, flag := range required {
		if !slices.Contains(args, flag) {
			t.Errorf("expected args to contain %q, got %v", flag, args)
		}
	}
}

func TestBuildBwrapArgsNetworkEnabled(t *testing.T) {
	cfg := config.ExecutorToolConfig{
		Network:            true,
		AllowedEnvPatterns: []string{"PATH"},
	}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)
	if slices.Contains(args, "--unshare-net") {
		t.Error("expected --unshare-net to be absent when network is enabled")
	}
}

func TestBuildBwrapArgsWorkspaceBinding(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	found := false
	for i := 0; i < len(args)-2; i++ {
		if args[i] == "--bind" && args[i+1] == "/data/u1/workspace" && args[i+2] == "/workspace" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected workspace bind /data/u1/workspace /workspace in args, got %v", args)
	}
}

func TestBuildBwrapArgsSkillDirectoryBindings(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	cases := []struct {
		flag, source, target string
	}{
		{"--ro-bind", "/skills/global", "/skills/global"},
		{"--ro-bind", "/data/u1/skills", "/skills/user"},
	}
	for _, c := range cases {
		idx := slices.Index(args, c.flag)
		if idx == -1 || idx+2 >= len(args) {
			t.Fatalf("expected %s flag with source and target", c.flag)
		}
		// Skip earlier ro-bind flags (host system dirs) to find the skill mounts.
		found := false
		for i := idx; i < len(args)-2; i++ {
			if args[i] == c.flag && args[i+1] == c.source && args[i+2] == c.target {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s %s %s in args, got %v", c.flag, c.source, c.target, args)
		}
	}
}

func TestBuildBwrapArgsSetsPYTHONPATH(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	found := false
	for i := 0; i < len(args)-2; i++ {
		if args[i] == "--setenv" && args[i+1] == "PYTHONPATH" && args[i+2] == "/workspace/.pip" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --setenv PYTHONPATH /workspace/.pip in args, got %v", args)
	}
}

func TestBuildBwrapArgsAppendsPYTHONPATH(t *testing.T) {
	t.Setenv("PYTHONPATH", "/usr/lib/python")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH", "PYTHON*"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	found := false
	for i := 0; i < len(args)-2; i++ {
		if args[i] == "--setenv" && args[i+1] == "PYTHONPATH" && args[i+2] == "/workspace/.pip:/usr/lib/python" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --setenv PYTHONPATH /workspace/.pip:/usr/lib/python in args, got %v", args)
	}
}

func TestBuildBwrapArgsPrependsWhenPYTHONPATHAllowedButEmpty(t *testing.T) {
	t.Setenv("PYTHONPATH", "")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PYTHON*"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	found := false
	for i := 0; i < len(args)-2; i++ {
		if args[i] == "--setenv" && args[i+1] == "PYTHONPATH" && args[i+2] == "/workspace/.pip" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --setenv PYTHONPATH /workspace/.pip in args, got %v", args)
	}
}

func TestBuildBwrapArgsClearsEnv(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH", "HOME"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", cfg)

	if !slices.Contains(args, "--clearenv") {
		t.Error("expected --clearenv flag")
	}
	for _, name := range []string{"PATH", "HOME"} {
		found := false
		for i := 0; i < len(args)-2; i++ {
			if args[i] == "--setenv" && args[i+1] == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected --setenv %s", name)
		}
	}
}
