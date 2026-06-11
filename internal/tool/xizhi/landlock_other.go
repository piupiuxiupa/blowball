//go:build !linux

package xizhi

import (
	"runtime"

	"github.com/lush/blowball/internal/pkg/logger"

	"go.uber.org/zap"
)

// applyLandlock is a no-op on non-Linux platforms (e.g. macOS dev machines).
// Landlock is a Linux-only kernel feature; on other platforms we log a warning
// so operators know process-level file restriction is inactive and rely on
// application-layer path validation alone.
func applyLandlock(dataDir string) error {
	logger.L().Warn("landlock not available on this platform; skipping process-level restriction",
		zap.String("data_dir", dataDir),
		zap.String("platform", runtime.GOOS),
	)
	return nil
}
