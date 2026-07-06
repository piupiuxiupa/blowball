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
	args := buildBwrapArgs("/data/u1/workspace", cfg)

	required := []string{
		"--unshare-user",
		"--unshare-ipc",
		"--unshare-pid",
		"--unshare-uts",
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/etc", "/etc",
		"--bind", "/data/u1/workspace", "/workspace",
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
	args := buildBwrapArgs("/data/u1/workspace", cfg)
	if slices.Contains(args, "--unshare-net") {
		t.Error("expected --unshare-net to be absent when network is enabled")
	}
}

func TestBuildBwrapArgsWorkspaceBinding(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/data/u1/workspace", cfg)

	idx := slices.Index(args, "--bind")
	if idx == -1 || idx+2 >= len(args) {
		t.Fatal("expected --bind flag with source and target")
	}
	if args[idx+1] != "/data/u1/workspace" || args[idx+2] != "/workspace" {
		t.Errorf("expected workspace bind /data/u1/workspace /workspace, got %v", args[idx:idx+3])
	}
}

func TestBuildBwrapArgsClearsEnv(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH", "HOME"}}
	args := buildBwrapArgs("/data/u1/workspace", cfg)

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
