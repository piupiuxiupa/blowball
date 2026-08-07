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

func TestDelete_File(t *testing.T) {
	root := t.TempDir()
	rel := "tmp/scratch.txt"
	absPath := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte("scratch"), 0o644))

	res, err := DeletePath(root, rel)
	require.NoError(t, err)
	got, ok := res.(deleteResult)
	require.True(t, ok)
	assert.Equal(t, rel, got.Path)
	assert.True(t, got.Deleted)
	assert.Equal(t, "file", got.Type)
	_, statErr := os.Stat(absPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestDelete_DirectoryRecursive(t *testing.T) {
	root := t.TempDir()
	rel := "tmp/scratch-dir"
	dirAbs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Join(dirAbs, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirAbs, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dirAbs, "sub", "b.txt"), []byte("b"), 0o644))

	res, err := DeletePath(root, rel)
	require.NoError(t, err)
	got, ok := res.(deleteResult)
	require.True(t, ok)
	assert.True(t, got.Deleted)
	assert.Equal(t, "directory", got.Type)
	_, statErr := os.Stat(dirAbs)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestDelete_NonExistent_Idempotent(t *testing.T) {
	root := t.TempDir()
	res, err := DeletePath(root, "tmp/never-existed.txt")
	require.NoError(t, err)
	got, ok := res.(deleteResult)
	require.True(t, ok)
	assert.False(t, got.Deleted)
	assert.Equal(t, "none", got.Type)
}

func TestDelete_OutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := DeletePath(root, "../../etc/passwd")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestDelete_AbsolutePath_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := DeletePath(root, "/etc/passwd")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestDelete_SymlinkEscape_Rejected(t *testing.T) {
	root := t.TempDir()
	// A file outside the workspace.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	// A symlink inside the workspace pointing outside.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "evil.txt")))

	_, err := DeletePath(root, "evil.txt")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
	// The outside target must be untouched.
	_, statErr := os.Stat(outside)
	assert.NoError(t, statErr)
}

func TestDelete_ReservedDirectory_Rejected(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".blowball", "skills", "foo"), 0o755))

	_, err := DeletePath(root, ".blowball/skills/foo")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
	// The reserved tree must be untouched.
	_, statErr := os.Stat(filepath.Join(root, ".blowball", "skills", "foo"))
	assert.NoError(t, statErr)
}

func TestDelete_ErrorIncludesRelativePathGuidance(t *testing.T) {
	root := t.TempDir()
	_, err := DeletePath(root, "/abs/path")
	require.Error(t, err)
	// The rejection message guides the model toward relative paths.
	assert.Contains(t, err.Error(), "relative path")
}

// TestRegisterAll_DeleteExecute_DecodesArgs exercises the registered tool's
// Execute callback end-to-end (arg decoding -> DeletePath) for xizhi_delete.
func TestRegisterAll_DeleteExecute_DecodesArgs(t *testing.T) {
	root := t.TempDir()
	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameDeleteFile)
	require.True(t, ok)

	rel := "tmp/scratch.txt"
	absPath := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte("x"), 0o644))

	args, err := json.Marshal(deleteArgs{Path: rel})
	require.NoError(t, err)
	res, err := spec.Execute(context.Background(), args)
	require.NoError(t, err)
	got, ok := res.(deleteResult)
	require.True(t, ok)
	assert.True(t, got.Deleted)
	assert.Equal(t, "file", got.Type)
}
