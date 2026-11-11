//go:build linux

package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lush/blowball/internal/config"
)

// TestBwrapCronNamespaceIsReadOnly verifies with a REAL bubblewrap sandbox that
// the `.cron` namespace is read-only inside every bash invocations's
// /workspace, while the rest of the workspace stays writable.
func TestBwrapCronNamespaceIsReadOnly(t *testing.T) {
	if !IsAvailable() {
		t.Skip("bwrap not available on this host")
	}
	ws := t.TempDir()
	cronDir := filepath.Join(ws, ".cron", "task-1", "draft")
	if err := os.MkdirAll(cronDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cronDir, "secret.txt"), []byte("pinned"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(ws, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	emptyTools := t.TempDir()

	args := buildBwrapArgs(ws, tmp, t.TempDir(), emptyTools, filepath.Join(ws, ".cron"),
		defaultSandbox(), config.ExecutorToolConfig{})
	script := `
set -e
cat /workspace/.cron/task-1/draft/secret.txt
if echo tampered > /workspace/.cron/task-1/draft/secret.txt 2>/dev/null; then
  echo "WRITE_SUCCEEDED"
  exit 9
fi
echo writable > /workspace/ordinary.txt
echo OK
`
	args = append(args, "/bin/sh", "-c", script)
	cmd := exec.Command("bwrap", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bwrap sandbox failed: %v\noutput: %s", err, out)
	}
	if string(out) != "pinned\nOK\n" {
		t.Fatalf("unexpected sandbox output: %q", out)
	}
	raw, rerr := os.ReadFile(filepath.Join(cronDir, "secret.txt"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(raw) != "pinned" {
		t.Fatalf("the read-only mount allowed a .cron write: %q", raw)
	}
	if _, serr := os.Stat(filepath.Join(ws, "ordinary.txt")); serr != nil {
		t.Fatalf("ordinary workspace write failed: %v", serr)
	}
}
