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

	// Apply landlock to the three runtime subdirs with the configurable system
	// read-only baseline (default), exactly as the server does. No extra RO dirs.
	if err := ApplyLandlock([]string{dataDir, logDir, skillsDir}, nil, config.DefaultLandlockSystemReadOnly()); err != nil {
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

// landlockToolsROChildEnv re-execs the test binary as a child for the
// tools-read-only landlock scenario. As with the rotation test, the
// process-global landlock restriction is confined to the child so it never
// leaks into the parent test process.
const landlockToolsROChildEnv = "BLOWBALL_TEST_LANDLOCK_TOOLS_RO"

// TestApplyLandlock_ToolsReadOnly guards D5: {data-dir}/tools is restricted
// read-only while {data-dir}/data, logs and skills remain read-write. The check
// re-execs the test binary as a child process that applies landlock and probes
// each directory; the parent asserts the child reported success.
func TestApplyLandlock_ToolsReadOnly(t *testing.T) {
	if os.Getenv(landlockToolsROChildEnv) == "1" {
		landlockToolsROChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestApplyLandlock_ToolsReadOnly")
	cmd.Env = append(os.Environ(), landlockToolsROChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("landlocked child failed: %v\noutput:\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("TOOLS_RO_OK")) {
		t.Fatalf("child did not report tools-read-only success; output:\n%s", out)
	}
}

func landlockToolsROChild(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	logDir := filepath.Join(root, "logs")
	skillsDir := filepath.Join(root, "skills")
	toolsDir := filepath.Join(root, "tools")
	for _, d := range []string{dataDir, logDir, skillsDir, toolsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Seed a file in tools so the read probe has something to read.
	if err := os.WriteFile(filepath.Join(toolsDir, "tool.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("seed tools file: %v", err)
	}

	// Apply landlock exactly as the server does: data/logs/skills read-write,
	// tools read-only (mirroring the in-sandbox --ro-bind as defense-in-depth),
	// plus the default stat-guarded system read-only baseline.
	if err := ApplyLandlock([]string{dataDir, logDir, skillsDir}, []string{toolsDir}, config.DefaultLandlockSystemReadOnly()); err != nil {
		t.Fatalf("ApplyLandlock: %v", err)
	}

	// Writing to tools must be denied by landlock (read-only path class).
	if err := os.WriteFile(filepath.Join(toolsDir, "evil"), []byte("x"), 0o644); err == nil {
		t.Fatalf("write to tools dir unexpectedly succeeded; landlock did not mark it read-only")
	}

	// Reading from tools must still succeed under the read-only restriction.
	if _, err := os.ReadFile(filepath.Join(toolsDir, "tool.txt")); err != nil {
		t.Fatalf("read from tools dir failed under read-only landlock: %v", err)
	}

	// Writing to the read-write runtime subdirs must still succeed.
	for _, d := range []string{dataDir, logDir, skillsDir} {
		if err := os.WriteFile(filepath.Join(d, "marker"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write to rw dir %s failed under landlock: %v", d, err)
		}
	}

	fmt.Println("TOOLS_RO_OK")
}

// landlockMissingROChildEnv re-execs the test binary as a child for the
// missing-system-baseline scenario. As with the other landlock scenarios, the
// process-global restriction is confined to the child so it never leaks into the
// parent test process.
const landlockMissingROChildEnv = "BLOWBALL_TEST_LANDLOCK_MISSING_RO"

// TestApplyLandlock_MissingSystemROSkipped guards D3: a system_read_only
// baseline entry that does not exist on the host (e.g. /lib64 on aarch64) is
// skipped with a warning rather than failing the restriction. The check re-execs
// the test binary as a child that applies landlock with the default baseline
// plus a non-existent path and asserts the restriction succeeds; the parent
// asserts the child reported success.
func TestApplyLandlock_MissingSystemROSkipped(t *testing.T) {
	if os.Getenv(landlockMissingROChildEnv) == "1" {
		landlockMissingROChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestApplyLandlock_MissingSystemROSkipped")
	cmd.Env = append(os.Environ(), landlockMissingROChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("landlocked child failed: %v\noutput:\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("MISSING_RO_OK")) {
		t.Fatalf("child did not report missing-baseline success; output:\n%s", out)
	}
}

func landlockMissingROChild(t *testing.T) {
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

	// Default baseline plus a deliberately non-existent path: the missing entry
	// must be skipped (warned), not fail the restriction.
	systemRO := append(config.DefaultLandlockSystemReadOnly(), "/this/path/does/not/exist")
	if err := ApplyLandlock([]string{dataDir, logDir, skillsDir}, nil, systemRO); err != nil {
		t.Fatalf("ApplyLandlock with a missing baseline entry: %v", err)
	}

	// RW runtime dirs must remain writable under the restriction.
	if err := os.WriteFile(filepath.Join(dataDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write to data dir failed under landlock: %v", err)
	}

	fmt.Println("MISSING_RO_OK")
}
