//go:build linux

package xizhi

import (
	"fmt"
	"os"

	"github.com/landlock-lsm/go-landlock/landlock"
	"github.com/lush/blowball/internal/pkg/logger"
	"go.uber.org/zap"
)

// applyLandlock restricts the current process: read+write access under each
// directory in rwDirs, read-only access under each directory in roDirs, and
// read-only access under each existing directory in systemRODirs. The V2 ABI is
// requested with BestEffort so the call still succeeds on older kernels that
// lack V2 features — they get whatever subset the kernel supports rather than
// failing the whole restriction.
//
// D5/D6 (see ApplyLandlock): rwDirs are the runtime subdirectories the process
// must write to ({d}/data, {d}/logs, {d}/skills) — covering the logs directory
// for lumberjack's post-rotation reopen — while roDirs holds operator-supplied
// static content ({d}/tools) that must be readable but never mutated.
// systemRODirs is the configurable system read-only baseline; D3 makes it
// stat-guarded so a missing entry (e.g. /lib64 on aarch64) is skipped with a
// warning rather than failing the restriction, matching probe.go's behavior.
// go-landlock composes multiple RO/RW restrictors in a single RestrictPaths call.
func applyLandlock(rwDirs, roDirs, systemRODirs []string) error {
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

	// Stat-guarded system read-only baseline (D3): only restrict entries that
	// actually exist, skipping (and warning on) the rest so a missing /lib64 on
	// aarch64 does not fail the restriction.
	systemROPaths := make([]string, 0, len(systemRODirs))
	for _, d := range systemRODirs {
		if d == "" {
			continue
		}
		if _, err := os.Stat(d); err != nil {
			logger.L().Warn("landlock: skipping missing system read-only baseline dir",
				zap.String("dir", d))
			continue
		}
		systemROPaths = append(systemROPaths, d)
	}

	rules := []landlock.Rule{landlock.RWDirs(rwPaths...)}
	if len(systemROPaths) > 0 {
		rules = append(rules, landlock.RODirs(systemROPaths...)) // read-only system files
	}
	if len(roPaths) > 0 {
		rules = append(rules, landlock.RODirs(roPaths...))
	}

	if err := landlock.V2.BestEffort().RestrictPaths(rules...); err != nil {
		return fmt.Errorf("landlock: restrict rw=%v ro=%v systemRO=%v: %w", rwPaths, roPaths, systemROPaths, err)
	}
	return nil
}
