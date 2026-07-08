package executor

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/lush/blowball/internal/config"
)

// available reports whether bubblewrap can be used on this platform.
// It is initialised once at package load time.
var available = detectBwrap()

// IsAvailable returns whether bubblewrap is installed and usable on the current
// platform. It is exported so callers (such as cmd/server/main.go) can decide
// whether to treat missing bwrap as a fatal error.
func IsAvailable() bool {
	return available
}

// detectBwrap checks for a working bwrap binary. On Linux this runs
// `bwrap --version`; on other platforms it returns false without invoking
// anything so development on macOS/Windows is unaffected.
func detectBwrap() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	cmd := exec.Command("bwrap", "--version")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// buildBwrapArgs constructs the bubblewrap argument list for a sandbox whose
// root filesystem exposes the minimum required host directories read-only, the
// user's workspace read-write at /workspace, the workspace's tmp directory
// bound to /tmp, and the global and per-user skill directories read-only at
// /skills/global and /skills/user.
func buildBwrapArgs(workspaceRoot, workspaceTmp, globalSkillsDir, userSkillsDir string, cfg config.ExecutorToolConfig) []string {
	args := []string{
		"--unshare-user",
		"--unshare-ipc",
		"--unshare-pid",
		"--unshare-uts",
		"--die-with-parent",
		"--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", workspaceTmp, "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/etc", "/etc",
		"--bind", workspaceRoot, "/workspace",
		"--ro-bind", globalSkillsDir, "/skills/global",
		"--ro-bind", userSkillsDir, "/skills/user",
		"--chdir", "/workspace",
	}

	if !cfg.Network {
		args = append(args, "--unshare-net")
	}

	args = append(args, "--clearenv")
	for k, v := range filterEnv(cfg.AllowedEnvPatterns) {
		args = append(args, "--setenv", k, v)
	}

	return args
}

// bwrapError describes why bubblewrap is unavailable.
type bwrapError struct {
	msg string
}

func (e *bwrapError) Error() string {
	return e.msg
}

// requireAvailable returns an error when executor tools are enabled but bwrap
// cannot be found on Linux. It returns nil on non-Linux platforms.
func requireAvailable() error {
	if available {
		return nil
	}
	if runtime.GOOS == "linux" {
		return fmt.Errorf("executor: bubblewrap (bwrap) is required on Linux but was not found in PATH")
	}
	return nil
}
