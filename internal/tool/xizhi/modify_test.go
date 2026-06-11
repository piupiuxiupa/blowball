package xizhi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFixture(t *testing.T, root, rel, body string) string {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	return abs
}

func TestModify_UniqueMatch_Replaces(t *testing.T) {
	root := t.TempDir()
	body := "func A() {}\nfunc B() {}\n"
	abs := writeFixture(t, root, "main.go", body)

	res, err := ModifyFile(root, "main.go", "func B() {}", "func B() int { return 1 }")
	require.NoError(t, err)
	got, ok := res.(modifyResult)
	require.True(t, ok)
	assert.Equal(t, "main.go", got.Path)
	assert.Equal(t, len(body), got.OldSize)

	want := "func A() {}\nfunc B() int { return 1 }\n"
	onDisk, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, want, string(onDisk))
}

func TestModify_NoMatch_ErrOldContentNotFound(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "main.go", "alpha beta\n")

	_, err := ModifyFile(root, "main.go", "gamma", "delta")
	if !errors.Is(err, ErrOldContentNotFound) {
		t.Fatalf("err = %v, want ErrOldContentNotFound", err)
	}
}

func TestModify_MultipleMatches_ErrOldContentAmbiguous(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "main.go", "todo\ntodo\ntodo\n")

	_, err := ModifyFile(root, "main.go", "todo", "done")
	if !errors.Is(err, ErrOldContentAmbiguous) {
		t.Fatalf("err = %v, want ErrOldContentAmbiguous", err)
	}
}

func TestModify_NonExistentFile_ErrFileNotFound(t *testing.T) {
	root := t.TempDir()
	_, err := ModifyFile(root, "ghost.txt", "a", "b")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

func TestModify_OutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := ModifyFile(root, "../../etc/passwd", "a", "b")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}
