package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// quietLog returns a logger whose output is discarded so health-check warnings
// do not clutter test output.
func quietLog(t *testing.T) *zap.Logger {
	t.Helper()
	return zap.NewNop()
}

func TestCheckSharedBackend_EmptyDataDirFails(t *testing.T) {
	if err := CheckSharedBackend(CheckOptions{DataDir: "", Log: quietLog(t)}); err == nil {
		t.Fatal("CheckSharedBackend expected error for empty data dir, got nil")
	}
}

func TestCheckSharedBackend_UnwritableFails(t *testing.T) {
	// A path that does not exist cannot be written to, deterministically and
	// regardless of whether the test runs as root.
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	err := CheckSharedBackend(CheckOptions{DataDir: dir, Log: quietLog(t)})
	if err == nil {
		t.Fatal("CheckSharedBackend expected error for unwritable/missing data dir, got nil")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error %q does not mention not writable", err.Error())
	}
}

func TestCheckSharedBackend_LeavesNoProbeFile(t *testing.T) {
	dir := t.TempDir()
	// Run twice; the second run would fail if the first left the probe file in
	// place and the check treated a stale file as a write success. (WriteFile
	// truncates, so this is mostly a regression guard for cleanup.)
	for range 2 {
		_ = CheckSharedBackend(CheckOptions{DataDir: dir, Log: quietLog(t)})
	}
	if _, err := os.Stat(filepath.Join(dir, probeFileName)); err == nil {
		t.Errorf("probe file %q was left behind in %q", probeFileName, dir)
	}
}

func TestCheckSharedBackend_LocalDir(t *testing.T) {
	// A plain local temp dir is writable but is NOT a FUSE-family filesystem.
	// On Linux the check must therefore reject it (this is exactly the "operator
	// forgot to mount JuiceFS" signal); on non-Linux the fstype guard is
	// unavailable, so the writable temp dir passes.
	dir := t.TempDir()
	err := CheckSharedBackend(CheckOptions{DataDir: dir, Log: quietLog(t)})
	switch runtime.GOOS {
	case "linux":
		if err == nil {
			t.Fatal("CheckSharedBackend expected error for local (non-FUSE) temp dir on Linux, got nil")
		}
		if !strings.Contains(err.Error(), "not on a shared POSIX filesystem") {
			t.Errorf("error %q does not mention shared POSIX filesystem", err.Error())
		}
	default:
		if err != nil {
			t.Fatalf("CheckSharedBackend on non-Linux writable temp dir returned error: %v", err)
		}
	}
}
