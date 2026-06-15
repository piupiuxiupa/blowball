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

func TestGlobFiles_RecursivePattern(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "nested", "b.go"), []byte("y"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "c.txt"), []byte("z"), 0o644))

	res, err := GlobFiles(root, ".", "src/**/*.go", false)
	require.NoError(t, err)
	got := res.(globResult)
	assert.ElementsMatch(t, []string{"src/a.go", "src/nested/b.go"}, got.Matches)
}

func TestGlobFiles_FromSubdirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "a_test.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "b.go"), []byte("y"), 0o644))

	res, err := GlobFiles(root, "internal", "**/*_test.go", false)
	require.NoError(t, err)
	got := res.(globResult)
	assert.Equal(t, []string{"a_test.go"}, got.Matches)
}

func TestGlobFiles_MatchesDirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cmd", "server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cmd", "main.go"), []byte("x"), 0o644))

	res, err := GlobFiles(root, ".", "cmd/*", false)
	require.NoError(t, err)
	got := res.(globResult)
	assert.Contains(t, got.Matches, "cmd/server")
}

func TestGlobFiles_NoMatches(t *testing.T) {
	root := t.TempDir()
	res, err := GlobFiles(root, ".", "**/*.missing", false)
	require.NoError(t, err)
	got := res.(globResult)
	assert.Empty(t, got.Matches)
}

func TestGlobFiles_PathOutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := GlobFiles(root, "../../etc", "*.go", false)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestGlobFiles_HiddenExcluded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "visible"), []byte("y"), 0o644))

	res, err := GlobFiles(root, ".", "**/*", false)
	require.NoError(t, err)
	got := res.(globResult)
	assert.Contains(t, got.Matches, "visible")
	assert.NotContains(t, got.Matches, ".git/config")
}

func TestGlobFiles_ViaRegistry_Execute(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o644))

	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameGlobFiles)
	require.True(t, ok)

	args, err := json.Marshal(globArgs{Path: ".", Pattern: "*.txt"})
	require.NoError(t, err)

	res, err := spec.Execute(t.Context(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"matches":["file.txt"]`)
}
