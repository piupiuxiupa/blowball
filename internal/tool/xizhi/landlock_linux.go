//go:build linux

package xizhi

import (
	"fmt"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// applyLandlock restricts the current process: read+write access under each
// directory in rwDirs, and read-only access under each directory in roDirs. The
// V2 ABI is requested with BestEffort so the call still succeeds on older
// kernels that lack V2 features — they get whatever subset the kernel supports
// rather than failing the whole restriction.
//
// D5/D6 (see ApplyLandlock): rwDirs are the runtime subdirectories the process
// must write to ({d}/data, {d}/logs, {d}/skills) — covering the logs directory
// for lumberjack's post-rotation reopen — while roDirs holds operator-supplied
// static content ({d}/tools) that must be readable but never mutated. go-landlock
// composes multiple RO/RW restrictors in a single RestrictPaths call.
func applyLandlock(rwDirs, roDirs []string) error {
	if len(rwDirs) == 0 {
		return fmt.Errorf("landlock: no read-write directories to restrict")
	}
	rwPaths := make([]string, 0, len(rwDirs))
	for i, d := range rwDirs {
		if d == "" {
			return fmt.Errorf("landlock: rwDirs[%d] is empty", i)
		}
		rwPaths = append(rwPaths, d)
	}
	roPaths := make([]string, 0, len(roDirs))
	for i, d := range roDirs {
		if d == "" {
			return fmt.Errorf("landlock: roDirs[%d] is empty", i)
		}
		roPaths = append(roPaths, d)
	}

	rules := []landlock.Rule{
		landlock.RODirs("/etc", "/usr", "/bin", "/lib", "/lib64", "/proc"), // allow read-only access to system files
		landlock.RWDirs(rwPaths...),
	}
	if len(roPaths) > 0 {
		rules = append(rules, landlock.RODirs(roPaths...))
	}

	if err := landlock.V2.BestEffort().RestrictPaths(rules...); err != nil {
		return fmt.Errorf("landlock: restrict rw=%v ro=%v: %w", rwPaths, roPaths, err)
	}
	return nil
}
