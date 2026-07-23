package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// probeTimeout bounds the startup bwrap self-check so a hung sandbox cannot
// stall boot indefinitely. A healthy probe finishes in well under a second.
const probeTimeout = 10 * time.Second

// probeMarker is the file the probe writes inside the bound /workspace to prove
// a user-namespace-mapped uid can write to the (FUSE) mount.
const probeMarker = ".jfs-allow-other-probe"

// systemROBindDirs are the host directories bound read-only into the probe
// sandbox so /bin/sh can run. Only directories that actually exist are bound,
// so the probe does not false-positive on architectures/distros that lack a
// given path (e.g. /lib64 on aarch64).
var systemROBindDirs = []string{"/usr", "/bin", "/lib", "/lib64"}

// ProbeFUSEWorkspace verifies that a user-namespace-mapped uid (the way bwrap
// runs sandboxed agents via --unshare-user) can WRITE to the shared filesystem
// mounted under dataDir. This catches the single most common shared-mode
// misconfiguration: JuiceFS mounted WITHOUT --allow-other (or without
// user_allow_other in /etc/fuse.conf), which leaves /workspace accessible only
// to the mounting uid and makes every agent bash/python call fail with EACCES
// at runtime. Running the probe at startup surfaces the problem before any agent
// turn does.
//
// The probe creates a throwaway workspace directory under dataDir (so it is on
// the shared FS), binds it at /workspace inside a minimal bwrap user namespace,
// and writes+removes a marker file. It is Linux-only and a no-op when bwrap is
// unavailable (the caller has already fatal-exited in that case when executor
// tools are enabled; if reached without bwrap we skip rather than fail).
//
// The dataDir MUST already be the shared-FS root the operator mounted; the
// caller runs this only in shared mode.
func ProbeFUSEWorkspace(dataDir string) error {
	if runtime.GOOS != "linux" || !available {
		return nil
	}
	if dataDir == "" {
		return fmt.Errorf("executor probe: data dir is empty")
	}

	probeWS, err := os.MkdirTemp(dataDir, ".jfs-probe-")
	if err != nil {
		return fmt.Errorf("create probe workspace under %q: %w", dataDir, err)
	}
	defer os.RemoveAll(probeWS)

	args := []string{
		"--unshare-user",
		"--unshare-pid",
		"--proc", "/proc",
		"--dev", "/dev",
		"--dir", "/tmp",
	}
	for _, dir := range systemROBindDirs {
		if _, statErr := os.Stat(dir); statErr == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}
	args = append(args,
		"--bind", probeWS, "/workspace",
		"--chdir", "/workspace",
		"--", "/bin/sh", "-c", "echo ok > /workspace/"+probeMarker+" && rm /workspace/"+probeMarker,
	)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		// The caller maps this to a fatal startup error with the --allow-other /
		// user_allow_other remediation hint; keep the underlying detail attached.
		return fmt.Errorf("mapped-uid could not write to the shared workspace at %q via bwrap: %w "+
			"(verify the JuiceFS mount uses --allow-other and /etc/fuse.conf has user_allow_other)",
			filepath.Join(dataDir, "<user>", "workspace"), err)
	}
	return nil
}
