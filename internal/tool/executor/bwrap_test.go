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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)
	if slices.Contains(args, "--unshare-net") {
		t.Error("expected --unshare-net to be absent when network is enabled")
	}
}

func TestBuildBwrapArgsWorkspaceBinding(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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
	args := buildBwrapArgs("/data/u1/workspace", "/data/u1/workspace/tmp", "/skills/global", "/data/u1/skills", "/data/tools", cfg)

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

// setenvValue returns the value of the --setenv KEY entry in args, or "" if the
// key is not set. HOME and PATH are each emitted at most once, so the first
// match is authoritative.
func setenvValue(args []string, key string) string {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == key {
			return args[i+2]
		}
	}
	return ""
}

// countSetenv returns how many --setenv KEY entries appear in args.
func countSetenv(args []string, key string) int {
	n := 0
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == key {
			n++
		}
	}
	return n
}

// findTriplet returns the index i where args[i], args[i+1], args[i+2] equal the
// given flag/src/target triple, or -1 if absent.
func findTriplet(args []string, flag, src, target string) int {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == src && args[i+2] == target {
			return i
		}
	}
	return -1
}

// 5.1: the synthetic home tmpfs must precede the operator-tools ro-bind so the
// mountpoint exists when the bind lands (D2 ordering).
func TestBuildBwrapArgsHomeTmpfsPrecedesToolsBind(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", "/data/tools", cfg)

	tmpfsIdx := slices.Index(args, "--tmpfs")
	if tmpfsIdx == -1 || tmpfsIdx+1 >= len(args) || args[tmpfsIdx+1] != sandboxHome {
		t.Fatalf("expected --tmpfs %s in args, got %v", sandboxHome, args)
	}
	bindIdx := findTriplet(args, "--ro-bind", "/data/tools", toolsBinPath)
	if bindIdx == -1 {
		t.Fatalf("expected --ro-bind /data/tools %s in args, got %v", toolsBinPath, args)
	}
	if bindIdx < tmpfsIdx {
		t.Errorf("expected --tmpfs (idx %d) to precede the tools --ro-bind (idx %d)", tmpfsIdx, bindIdx)
	}
}

// 5.2 + 4.4: HOME is forced to the synthetic path exactly once even when HOME is
// allowed (host HOME would otherwise leak and point at an unmounted path).
func TestBuildBwrapArgsForcesHomeWhenAllowed(t *testing.T) {
	t.Setenv("HOME", "/home/hostuser")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"HOME", "PATH"}}
	args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", "/data/tools", cfg)

	if home := setenvValue(args, "HOME"); home != sandboxHome {
		t.Errorf("expected HOME forced to %q, got %q", sandboxHome, home)
	}
	if n := countSetenv(args, "HOME"); n != 1 {
		t.Errorf("expected exactly one --setenv HOME, got %d", n)
	}
}

// 5.2: HOME is forced to the synthetic path even when HOME is filtered out of
// the inherited env (D3 — HOME is forced, not inherited).
func TestBuildBwrapArgsForcesHomeWhenFilteredOut(t *testing.T) {
	t.Setenv("HOME", "/home/hostuser")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}} // HOME not allowed
	args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", "/data/tools", cfg)

	if home := setenvValue(args, "HOME"); home != sandboxHome {
		t.Errorf("expected HOME forced to %q even when filtered out, got %q", sandboxHome, home)
	}
	if n := countSetenv(args, "HOME"); n != 1 {
		t.Errorf("expected exactly one --setenv HOME, got %d", n)
	}
}

// 5.3: $HOME/.local/bin is the first PATH entry, with the inherited host PATH
// appended when PATH is allowed (D4).
func TestBuildBwrapArgsPrependsToolsBinToPathWhenAllowed(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", "/data/tools", cfg)

	want := toolsBinPath + ":/usr/bin:/bin"
	if path := setenvValue(args, "PATH"); path != want {
		t.Errorf("expected PATH %q, got %q", want, path)
	}
}

// 5.3: when PATH is filtered out, the sandbox still gets the tools bin as PATH
// (the host PATH the operator dropped is not re-injected).
func TestBuildBwrapArgsToolsBinOnlyWhenPathFilteredOut(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"HOME"}} // PATH not allowed
	args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", "/data/tools", cfg)

	if path := setenvValue(args, "PATH"); path != toolsBinPath {
		t.Errorf("expected PATH %q when host PATH filtered out, got %q", toolsBinPath, path)
	}
}

// 5.4: an empty or populated tools dir both emit the tmpfs home and the
// ro-bind. buildBwrapArgs always emits both (no conditional), and bwrap binds
// an empty directory without error, so an empty operator tools dir is harmless.
func TestBuildBwrapArgsEmitsHomeAndToolsBindRegardlessOfToolsDir(t *testing.T) {
	cfg := config.ExecutorToolConfig{AllowedEnvPatterns: []string{"PATH"}}
	for _, toolsDir := range []string{"", "/data/tools"} {
		args := buildBwrapArgs("/ws", "/ws/tmp", "/skills/global", "/skills/user", toolsDir, cfg)

		tmpfsIdx := slices.Index(args, "--tmpfs")
		if tmpfsIdx == -1 || tmpfsIdx+1 >= len(args) || args[tmpfsIdx+1] != sandboxHome {
			t.Errorf("toolsDir=%q: expected --tmpfs %s, got %v", toolsDir, sandboxHome, args)
		}
		if findTriplet(args, "--ro-bind", toolsDir, toolsBinPath) == -1 {
			t.Errorf("toolsDir=%q: expected --ro-bind %q %s, got %v", toolsDir, toolsDir, toolsBinPath, args)
		}
	}
}
