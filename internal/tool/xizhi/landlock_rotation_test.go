//go:build linux

package xizhi

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"go.uber.org/zap"
)

// rotationChildEnv re-execs the test binary as a child so the process-global
// landlock restriction stays confined to the child and never leaks into the
// parent test process (which would break every other test in the package).
const rotationChildEnv = "BLOWBALL_TEST_LANDLOCK_ROTATION"

// TestApplyLandlock_RotationReopens guards D6: after landlock is applied to the
// runtime subdirectories, a lumberjack rotation (which renames and reopens the
// log file) must still succeed because {d}/logs is inside the sandbox. The check
// re-execs the test binary as a child process that applies landlock and triggers
// a rotation; the parent asserts the child reported success.
func TestApplyLandlock_RotationReopens(t *testing.T) {
	if os.Getenv(rotationChildEnv) == "1" {
		landlockRotationChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestApplyLandlock_RotationReopens")
	cmd.Env = append(os.Environ(), rotationChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("landlocked child failed: %v\noutput:\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("ROTATION_OK")) {
		t.Fatalf("child did not report rotation success; output:\n%s", out)
	}
}

func landlockRotationChild(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	logDir := filepath.Join(root, "logs")
	skillsDir := filepath.Join(root, "skills")
	for _, d := range []string{dataDir, logDir, skillsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Apply landlock to the three runtime subdirs, exactly as the server does.
	if err := ApplyLandlock(dataDir, logDir, skillsDir); err != nil {
		t.Fatalf("ApplyLandlock: %v", err)
	}

	// Build a logger whose file sink rotates at 1 MiB.
	log, err := logger.Init(config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: []string{"file"},
		File: config.LogFileConfig{
			MaxSizeMB:  1,
			MaxBackups: 3,
		},
	}, logDir)
	if err != nil {
		t.Fatalf("logger.Init: %v", err)
	}

	// Write enough to cross the 1 MiB threshold and force a rotation. This is
	// the reopen-after-sandbox path the test exists to guard.
	blob := strings.Repeat("a", 600*1024) // 600 KiB per entry
	for i := 0; i < 4; i++ {              // 4 * 600 KiB > 1 MiB
		log.Info("fill", zap.String("blob", blob))
	}
	_ = log.Sync()

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", logDir, err)
	}
	var rotated bool
	for _, e := range entries {
		if e.Name() != logger.LogFileName && strings.HasPrefix(e.Name(), "blowball-") && strings.HasSuffix(e.Name(), ".log") {
			rotated = true
		}
	}
	if !rotated {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no rotated backup under landlock; entries=%v", names)
	}

	// Active log file must still be writable post-rotation.
	log.Info("post-rotation marker")
	_ = log.Sync()
	active, err := os.ReadFile(filepath.Join(logDir, logger.LogFileName))
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	if !strings.Contains(string(active), "post-rotation marker") {
		t.Fatalf("active log not written after rotation; content=%q", active)
	}

	fmt.Println("ROTATION_OK")
}
