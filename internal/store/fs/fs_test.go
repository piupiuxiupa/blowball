package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestWriteSession_ReadBack(t *testing.T) {
	store, root := newTestStore(t)
	ctx := context.Background()

	want := []byte(`{"session_id":"s-1","messages":[]}`)
	if err := store.WriteSession(ctx, "u-1", "s-1", want); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	// The expected path is {root}/{userID}/sessions/{sessionID}.json.
	dst := filepath.Join(root, "u-1", "sessions", "s-1.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read file %q: %v", dst, err)
	}
	if string(got) != string(want) {
		t.Fatalf("file content = %q, want %q", got, want)
	}

	// ReadSession must return the same bytes via the API.
	gotAPI, err := store.ReadSession(ctx, "u-1", "s-1")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if string(gotAPI) != string(want) {
		t.Fatalf("ReadSession = %q, want %q", gotAPI, want)
	}
}

func TestReadSession_MissingReturnsNil(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	got, err := store.ReadSession(ctx, "u-1", "never-written")
	if err != nil {
		t.Fatalf("ReadSession on missing file returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("ReadSession on missing file = %q, want nil", got)
	}
}

func TestDeleteSession_Idempotent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Delete a file that was never written — must be a no-op.
	if err := store.DeleteSession(ctx, "u-1", "ghost"); err != nil {
		t.Fatalf("DeleteSession on missing file: %v", err)
	}

	// Write then delete then delete again.
	if err := store.WriteSession(ctx, "u-1", "s-1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, "u-1", "s-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	// Second delete on the now-removed file must still succeed.
	if err := store.DeleteSession(ctx, "u-1", "s-1"); err != nil {
		t.Fatalf("DeleteSession (idempotent): %v", err)
	}

	got, err := store.ReadSession(ctx, "u-1", "s-1")
	if err != nil {
		t.Fatalf("ReadSession after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("ReadSession after delete = %q, want nil", got)
	}
}

func TestWriteSession_OverwritesExisting(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.WriteSession(ctx, "u-1", "s-1", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteSession(ctx, "u-1", "s-1", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadSession(ctx, "u-1", "s-1")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("ReadSession = %q, want %q", got, "second")
	}
}

func TestEnsureUserDirs_CreatesAllSubdirs(t *testing.T) {
	store, root := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureUserDirs(ctx, "u-1"); err != nil {
		t.Fatalf("EnsureUserDirs: %v", err)
	}

	for _, sub := range []string{"sessions", "workspace", "skills"} {
		dir := filepath.Join(root, "u-1", sub)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("subdir %q not created: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q exists but is not a directory", dir)
		}
	}

	// UserDirExists must now report true.
	if !store.UserDirExists("u-1") {
		t.Fatal("UserDirExists reported false right after EnsureUserDirs")
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
