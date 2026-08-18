package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore returns an FS Store rooted in a fresh temp dir along with the
// cleanup function the caller must defer.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatalf("New(%q): %v", dir, err)
	}
	return store, dir
}

func TestNew_CreatesRootIfMissing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "does-not-exist-yet")
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition failed: %q already exists", root)
	}
	if _, err := New(root); err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("root not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("root %q is not a directory", root)
	}
}

func TestNew_EmptyRootRejected(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") must be rejected")
	}
}

func TestEnsureUserDirs_CreatesWorkspaceLayout(t *testing.T) {
	store, root := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureUserDirs(ctx, "u-1"); err != nil {
		t.Fatalf("EnsureUserDirs: %v", err)
	}

	// workspace/ is the only top-level sibling (the sessions/ warm tier is
	// gone with the Redis-first persistence change); per-user skills are
	// nested under the workspace in the reserved .blowball/skills namespace.
	dir := filepath.Join(root, "u-1", "workspace")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("subdir %q not created: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q exists but is not a directory", dir)
	}

	// No sessions/ directory must be created anymore.
	if _, err := os.Stat(filepath.Join(root, "u-1", "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sessions/ must not be created, got err=%v", err)
	}

	// Per-user skills live under the workspace at .blowball/skills, not as a
	// top-level sibling.
	skillsDir := filepath.Join(root, "u-1", "workspace", ".blowball", "skills")
	if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
		t.Fatalf("reserved skills dir %q not created: %v", skillsDir, err)
	}

	// No top-level skills/ directory must be created.
	if _, err := os.Stat(filepath.Join(root, "u-1", "skills")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("top-level skills/ must not be created, got err=%v", err)
	}

	// UserDirExists must now report true.
	if !store.UserDirExists("u-1") {
		t.Fatal("UserDirExists reported false right after EnsureUserDirs")
	}
}

func TestEnsureUserDirs_EmptyUserRejected(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.EnsureUserDirs(context.Background(), ""); err == nil {
		t.Fatal("EnsureUserDirs(\"\") must be rejected")
	}
}

func TestUserSkillsLivesUnderWorkspace(t *testing.T) {
	store, root := newTestStore(t)

	got := store.UserSkills("u-1")
	want := filepath.Join(root, "u-1", "workspace", ".blowball", "skills")
	if got != want {
		t.Fatalf("UserSkills = %q, want %q", got, want)
	}
	// The skills dir must be a descendant of the workspace dir.
	ws := store.UserWorkspace("u-1")
	if !strings.HasPrefix(got, ws+string(filepath.Separator)) {
		t.Fatalf("UserSkills %q is not beneath workspace %q", got, ws)
	}
}

func TestEnsureUserDirs_Idempotent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureUserDirs(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	// Calling again on the existing layout must not fail.
	if err := store.EnsureUserDirs(ctx, "u-1"); err != nil {
		t.Fatalf("EnsureUserDirs (second call): %v", err)
	}
}

func TestUserDirExists_FalseWhenMissing(t *testing.T) {
	store, _ := newTestStore(t)
	if store.UserDirExists("never-created") {
		t.Fatal("UserDirExists reported true for never-created user")
	}
}

func TestUserWorkspacePathLayout(t *testing.T) {
	store, root := newTestStore(t)
	got := store.UserWorkspace("u-1")
	want := filepath.Join(root, "u-1", "workspace")
	if got != want {
		t.Fatalf("UserWorkspace = %q, want %q", got, want)
	}
}
