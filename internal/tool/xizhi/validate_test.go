package xizhi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_PathTraversal_Blocked(t *testing.T) {
	root := t.TempDir()

	cases := []string{
		"../../etc/passwd",
		"../escape",
		"foo/../../../etc",
		"..",
	}
	for _, rel := range cases {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			_, err := validatePath(root, rel)
			if err == nil {
				t.Fatalf("validatePath(%q) returned nil error, want ErrPathOutsideWorkspace", rel)
			}
			if !errors.Is(err, ErrPathOutsideWorkspace) {
				t.Fatalf("validatePath(%q) err = %v, want wrapping ErrPathOutsideWorkspace", rel, err)
			}
		})
	}
}

func TestValidate_AbsolutePath_Blocked(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "outside.txt")

	_, err := validatePath(root, abs)
	if err == nil {
		t.Fatalf("validatePath(absolute) returned nil error")
	}
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestValidate_EmptyInputs_Blocked(t *testing.T) {
	root := t.TempDir()
	if _, err := validatePath(root, ""); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("empty relPath err = %v, want ErrPathOutsideWorkspace", err)
	}
	if _, err := validatePath("", "foo"); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("empty root err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestValidate_SymlinkEscape_Blocked(t *testing.T) {
	root := t.TempDir()

	// Outside target the symlink will point to.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Create a symlink inside workspace pointing outside.
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := validatePath(root, "escape.txt")
	if err == nil {
		t.Fatalf("validatePath via escaping symlink returned nil error")
	}
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestValidate_SymlinkEscape_ToOutsideDir_Blocked(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// Place a real file in the outside dir so we know resolution would succeed.
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	// Symlink a subdirectory of the workspace to the outside dir, then ask
	// for a file through it.
	linkDir := filepath.Join(root, "leakdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	_, err := validatePath(root, filepath.Join("leakdir", "secret.txt"))
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestValidate_ValidPath_OK(t *testing.T) {
	root := t.TempDir()
	// Pre-create a subdirectory so EvalSymlinks on the parent has something to
	// resolve.
	sub := filepath.Join(root, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	abs, err := validatePath(root, "src/main.go")
	if err != nil {
		t.Fatalf("validatePath: %v", err)
	}
	want := filepath.Join(root, "src", "main.go")
	if abs != want {
		t.Fatalf("abs = %q, want %q", abs, want)
	}
}

func TestValidate_WorkspaceRootNotSiblingPrefix(t *testing.T) {
	// Regression: a workspace root /data/userA must not allow access to
	// /data/userA-extra via a naive HasPrefix check.
	parent := t.TempDir()
	rootA := filepath.Join(parent, "userA")
	rootAExtra := filepath.Join(parent, "userA-extra")
	for _, d := range []string{rootA, rootAExtra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", d, err)
		}
	}
	// The file we will try to reach lives inside the sibling.
	if err := os.WriteFile(filepath.Join(rootAExtra, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Asking for "../userA-extra/secret.txt" from rootA must be rejected as
	// path traversal.
	if _, err := validatePath(rootA, "../userA-extra/secret.txt"); !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("sibling-prefix traversal err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestIsPathOutsideWorkspace(t *testing.T) {
	if !IsPathOutsideWorkspace(ErrPathOutsideWorkspace) {
		t.Fatal("IsPathOutsideWorkspace(ErrPathOutsideWorkspace) = false")
	}
	if !IsPathOutsideWorkspace(fmt.Errorf("wrap: %w", ErrPathOutsideWorkspace)) {
		t.Fatal("IsPathOutsideWorkspace on wrapped err = false")
	}
	if IsPathOutsideWorkspace(errors.New("other")) {
		t.Fatal("IsPathOutsideWorkspace on unrelated err = true")
	}
}
