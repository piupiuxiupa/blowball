//go:build linux

package xizhi

import (
	"fmt"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// applyLandlock restricts the current process to read+write access under each
// directory in dirs. The V2 ABI is requested with BestEffort so the call still
// succeeds on older kernels that lack V2 features — they get whatever subset the
// kernel supports rather than failing the whole restriction.
//
// D6 (see ApplyLandlock): dirs are the specific runtime subdirectories
// ({d}/data, {d}/logs, {d}/skills) so the sandbox stays tight while still
// covering the logs directory for lumberjack's post-rotation reopen. go-landlock
// accepts multiple RWDirs in one call.
func applyLandlock(dirs []string) error {
	if len(dirs) == 0 {
		return fmt.Errorf("landlock: no directories to restrict")
	}
	rwPaths := make([]string, 0, len(dirs))
	for i, d := range dirs {
		if d == "" {
			return fmt.Errorf("landlock: dirs[%d] is empty", i)
		}
		rwPaths = append(rwPaths, d)
	}
	if err := landlock.V2.BestEffort().RestrictPaths(
		landlock.RODirs("/etc", "/usr", "/bin", "/lib", "/lib64", "/proc"), // allow read-only access to system files
		landlock.RWDirs(rwPaths...),
	); err != nil {
		return fmt.Errorf("landlock: restrict %v: %w", rwPaths, err)
	}
	return nil
}
