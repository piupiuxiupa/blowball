package xizhi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrite_NewFile_AutoMkdir(t *testing.T) {
	root := t.TempDir()
	content := "package main\n"

	res, err := WriteFile(root, "src/deep/nested/main.go", content)
	require.NoError(t, err)

	got, ok := res.(writeResult)
	require.True(t, ok, "result must be writeResult")
	assert.Equal(t, "src/deep/nested/main.go", got.Path)
	assert.Equal(t, len(content), got.Size)
	assert.Equal(t, filepath.Join(root, "src", "deep", "nested", "main.go"), got.Absolute)

	// File must exist on disk with auto-created parents and the right content.
	onDisk, err := os.ReadFile(got.Absolute)
	require.NoError(t, err)
	assert.Equal(t, content, string(onDisk))

	// Parent dirs created with 0o755.
	info, err := os.Stat(filepath.Join(root, "src", "deep", "nested"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestWrite_Overwrite(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(target, []byte("first"), 0o644))

	res, err := WriteFile(root, "file.txt", "second")
	require.NoError(t, err)
	got := res.(writeResult)
	assert.Equal(t, len("second"), got.Size)

	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "second", string(onDisk))
}

func TestWrite_PathOutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := WriteFile(root, "../../etc/foo", "x")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestWrite_PathTraversal_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := WriteFile(root, "../../etc/foo", "x")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestWrite_AbsolutePath_Rejected(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "outside.txt")
	_, err := WriteFile(root, abs, "x")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestWrite_ViaRegistry_Execute(t *testing.T) {
	// Ensure the registry Execute wrapper marshals args and returns the
	// writeResult as a JSON-serializable value.
	root := t.TempDir()

	// RegisterAll lives in register.go but we want a focused test on the
	// write Execute callback; call it via RegisterAll into a fresh registry.
	r := newTestRegistry(t)
	RegisterAll(r, root)

	spec, ok := r.Get(NameWriteFile)
	require.True(t, ok)

	args, err := json.Marshal(writeArgs{Path: "a/b.txt", Content: "hello"})
	require.NoError(t, err)

	res, err := spec.Execute(context.Background(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"path":"a/b.txt"`)
	assert.Contains(t, string(b), `"size":5`)
}
