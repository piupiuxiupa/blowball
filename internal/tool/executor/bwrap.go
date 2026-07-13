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

// pipTargetPath is the in-sandbox location where pip_install writes packages.
// It is bound to {workspaceRoot}/.pip on the host and prepended to PYTHONPATH
// so the python tool can import installed packages without sys.path changes.
const pipTargetPath = "/workspace/.pip"

// sandboxHome is the synthetic, in-namespace home directory established as a
// writable tmpfs for every sandboxed command. It is a fixed path that does not
// collide with any host home and does not require the host home to exist (see
// design D2). HOME is forced to this value regardless of allowed_env_patterns
// (D3), and the operator tools dir is bound read-only under it.
const sandboxHome = "/home/blowball"

// toolsBinPath is the in-sandbox location where the operator's tools dir is
// mounted read-only. Tools that hardcode $HOME/.local/bin find their binaries
// here, and it is prepended to PATH so the tools are invocable by bare name
// (D1/D4).
const toolsBinPath = sandboxHome + "/.local/bin"

// buildBwrapArgs constructs the bubblewrap argument list for a sandbox whose
// root filesystem exposes the minimum required host directories read-only, the
// user's workspace read-write at /workspace, the workspace's tmp directory
// bound to /tmp, the global and per-user skill directories read-only at
// /skills/global and /skills/user, and a writable synthetic home (tmpfs) with
// the operator tools dir bound read-only at $HOME/.local/bin.
func buildBwrapArgs(workspaceRoot, workspaceTmp, globalSkillsDir, userSkillsDir, toolsDir string, cfg config.ExecutorToolConfig) []string {
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
		// D2: establish the writable synthetic home as a tmpfs BEFORE binding the
		// operator tools under it, so the mountpoint exists when the ro-bind lands.
		"--tmpfs", sandboxHome,
		"--ro-bind", toolsDir, toolsBinPath,
		"--chdir", "/workspace",
	}

	if !cfg.Network {
		args = append(args, "--unshare-net")
	}

	args = append(args, "--clearenv")
	env := filterEnv(cfg.AllowedEnvPatterns)
	if existing, ok := env["PYTHONPATH"]; ok && existing != "" {
		env["PYTHONPATH"] = pipTargetPath + ":" + existing
	} else {
		env["PYTHONPATH"] = pipTargetPath
	}
	// D3: force HOME to the synthetic sandbox home, overriding any host HOME that
	// allowed_env_patterns would otherwise leak. Overwriting the map key yields
	// exactly one --setenv HOME regardless of whether HOME was allowed.
	env["HOME"] = sandboxHome
	// D4: prepend the operator tools bin to PATH so the operator's tools take
	// precedence over host /usr/bin. When PATH was filtered out, expose only the
	// tools bin rather than injecting the host PATH the operator chose to drop.
	if existing, ok := env["PATH"]; ok && existing != "" {
		env["PATH"] = toolsBinPath + ":" + existing
	} else {
		env["PATH"] = toolsBinPath
	}
	for k, v := range env {
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
