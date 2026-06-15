package xizhi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListFiles_Root(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o644))

	res, err := ListFiles(root, "", false)
	require.NoError(t, err)
	got := res.(listResult)
	assert.Equal(t, ".", got.Path)
	assert.Len(t, got.Entries, 2)
	assert.Equal(t, "a.txt", got.Entries[0].Name)
	assert.Equal(t, "file", got.Entries[0].Type)
	assert.Equal(t, int64(5), got.Entries[0].Size)
	assert.Equal(t, "sub", got.Entries[1].Name)
	assert.Equal(t, "dir", got.Entries[1].Type)
}

func TestListFiles_Subdirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644))

	res, err := ListFiles(root, "src", false)
	require.NoError(t, err)
	got := res.(listResult)
	assert.Equal(t, "src", got.Path)
	require.Len(t, got.Entries, 1)
	assert.Equal(t, "main.go", got.Entries[0].Name)
	assert.Equal(t, "file", got.Entries[0].Type)
}

func TestListFiles_IncludeHidden(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "visible"), []byte("y"), 0o644))

	res, err := ListFiles(root, ".", true)
	require.NoError(t, err)
	got := res.(listResult)
	require.Len(t, got.Entries, 2)
}

func TestListFiles_PathOutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := ListFiles(root, "../../etc/passwd", false)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestListFiles_NonExistentDirectory(t *testing.T) {
	root := t.TempDir()
	_, err := ListFiles(root, "missing", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found")
}

func TestListFiles_ViaRegistry_Execute(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o644))

	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameListFiles)
	require.True(t, ok)

	args, err := json.Marshal(listArgs{Path: "."})
	require.NoError(t, err)

	res, err := spec.Execute(t.Context(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"name":"file.txt"`)
}
