package xizhi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRead_ExistingFile(t *testing.T) {
	root := t.TempDir()
	rel := "src/main.go"
	absPath := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte("package main"), 0o644))

	res, err := ReadFile(root, rel)
	require.NoError(t, err)
	got, ok := res.(readResult)
	require.True(t, ok)
	assert.Equal(t, rel, got.Path)
	assert.Equal(t, "package main", got.Content)
	assert.Equal(t, len("package main"), got.Size)
}

func TestRead_NonExistentFile_ErrFileNotFound(t *testing.T) {
	root := t.TempDir()
	_, err := ReadFile(root, "nope.txt")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
	if !IsFileNotFound(err) {
		t.Fatal("IsFileNotFound returned false on ErrFileNotFound")
	}
}

func TestRead_OutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := ReadFile(root, "../../etc/passwd")
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}
