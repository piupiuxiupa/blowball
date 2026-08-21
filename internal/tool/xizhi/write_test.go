package xizhi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrite_NewFile_AutoMkdir(t *testing.T) {
	root := t.TempDir()
	content := "package main\n"

	res, err := WriteFile(root, "src/deep/nested/main.go", content, false)
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

	res, err := WriteFile(root, "file.txt", "second", false)
	require.NoError(t, err)
	got := res.(writeResult)
	assert.Equal(t, len("second"), got.Size)

	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "second", string(onDisk))
}

func TestWrite_PathOutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := WriteFile(root, "../../etc/foo", "x", false)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestWrite_PathTraversal_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := WriteFile(root, "../../etc/foo", "x", false)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestWrite_AbsolutePath_Rejected(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "outside.txt")
	_, err := WriteFile(root, abs, "x", false)
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
	RegisterAll(r, root, testXizhiConfig())

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

// llm-length-continuation prevention side: the append mode is what makes the
// "split large content into multiple smaller writes" steering executable —
// write_file's default is create-or-overwrite, so chunked writes need append
// semantics to compose (see the xizhi-tools delta spec).

func TestWrite_AppendToExistingFile(t *testing.T) {
	root := t.TempDir()
	first, second := "package main\n\nfunc main() {\n", "\tprintln(\"hi\")\n}\n"

	res1, err := WriteFile(root, "src/main.go", first, false)
	require.NoError(t, err)
	assert.Equal(t, len(first), res1.(writeResult).Size)
	assert.Equal(t, len(first), res1.(writeResult).Appended)

	res2, err := WriteFile(root, "src/main.go", second, true)
	require.NoError(t, err)
	got := res2.(writeResult)
	// Original content preserved; size is the post-write TOTAL, appended is
	// this call's chunk length.
	assert.Equal(t, len(first)+len(second), got.Size)
	assert.Equal(t, len(second), got.Appended)

	onDisk, err := os.ReadFile(filepath.Join(root, "src", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, first+second, string(onDisk))
}

func TestWrite_AppendCreatesMissingFile(t *testing.T) {
	root := t.TempDir()
	content := "from nothing\n"

	res, err := WriteFile(root, "docs/new/notes.md", content, true)
	require.NoError(t, err)
	got := res.(writeResult)
	assert.Equal(t, len(content), got.Size)
	assert.Equal(t, len(content), got.Appended)

	onDisk, err := os.ReadFile(filepath.Join(root, "docs", "new", "notes.md"))
	require.NoError(t, err)
	assert.Equal(t, content, string(onDisk))
}

func TestWrite_AppendOutsideWorkspaceBlocked(t *testing.T) {
	root := t.TempDir()
	_, err := WriteFile(root, "../../etc/passwd", "x", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path outside workspace")
}

// TestWrite_ExecuteModeValidation drives the registry Execute closure: the
// default (omitted) mode and both explicit values pass, anything else is a
// parse-level error that never touches the file system.
func TestWrite_ExecuteModeValidation(t *testing.T) {
	root := t.TempDir()
	spec := toolSpecForWrite(t, root)

	valid := map[string]string{"": "write", `"write"`: "write", `"append"`: "append"}
	for modeRaw := range valid {
		args := fmt.Sprintf(`{"path":"m.txt","content":"x","mode":%s}`, modeRaw)
		if modeRaw == "" {
			args = `{"path":"m.txt","content":"x"}`
		}
		_, err := spec.Execute(context.Background(), json.RawMessage(args))
		require.NoError(t, err, "mode %s must be accepted", modeRaw)
	}

	_, err := spec.Execute(context.Background(), json.RawMessage(`{"path":"m.txt","content":"x","mode":"patch"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid mode "patch"`)
}

// toolSpecForWrite registers the xizhi family into a scratch registry and
// returns the xizhi_write_file spec.
func toolSpecForWrite(t *testing.T, root string) *tool.ToolSpec {
	t.Helper()
	reg := tool.NewRegistry()
	RegisterAll(reg, root, testXizhiConfig())
	for _, spec := range reg.List() {
		if spec.Name == NameWriteFile {
			return spec
		}
	}
	t.Fatalf("xizhi_write_file not registered")
	return nil
}
