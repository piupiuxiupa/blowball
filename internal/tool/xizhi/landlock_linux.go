//go:build linux

package xizhi

import (
	"fmt"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// applyLandlock restricts the current process to read+write access under
// dataDir. The V2 ABI is requested with BestEffort so the call still succeeds
// on older kernels that lack V2 features — they get whatever subset the kernel
// supports rather than failing the whole restriction.
func applyLandlock(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("landlock: dataDir is empty")
	}
	if err := landlock.V2.BestEffort().RestrictPaths(landlock.RWDirs(dataDir)); err != nil {
		return fmt.Errorf("landlock: restrict %q: %w", dataDir, err)
	}
	return nil
}
