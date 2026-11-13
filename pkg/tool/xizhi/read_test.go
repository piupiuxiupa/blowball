package xizhi

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRead_ExistingFile_CatN(t *testing.T) {
	root := t.TempDir()
	rel := "src/main.go"
	absPath := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
	require.NoError(t, os.WriteFile(absPath, []byte("package main"), 0o644))

	res, err := ReadFile(root, rel, 0, 0)
	require.NoError(t, err)
	got, ok := res.(readResult)
	require.True(t, ok)
	assert.Equal(t, rel, got.Path)
	// Content is cat -n: "<lineNo>\t<line>".
	assert.Equal(t, "1\tpackage main", got.Content)
	assert.Equal(t, 1, got.StartLine)
	assert.Equal(t, 1, got.TotalLines)
	assert.Equal(t, len("package main"), got.Size)
	assert.False(t, got.Truncated)
}

func TestRead_MultiLine_CatN_LineNumbers(t *testing.T) {
	root := t.TempDir()
	content := "package main\n\nfunc main() {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0o644))

	res, err := ReadFile(root, "main.go", 0, 0)
	require.NoError(t, err)
	got := res.(readResult)
	// A trailing newline is not an extra line: 3 lines, numbered 1..3.
	assert.Equal(t, 3, got.TotalLines)
	assert.Equal(t, "1\tpackage main\n2\t\n3\tfunc main() {}", got.Content)
	assert.False(t, got.Truncated)
}

func TestRead_OffsetLimit_Paginates(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	// Read lines 10..19 (offset=10, limit=10).
	res, err := ReadFile(root, "big.txt", 10, 10)
	require.NoError(t, err)
	got := res.(readResult)
	assert.Equal(t, 50, got.TotalLines)
	assert.Equal(t, 10, got.StartLine)
	assert.True(t, got.Truncated, "more lines remain beyond the window")
	// Content is lines 10..19 cat -n.
	want := "10\tline10\n11\tline11\n12\tline12\n13\tline13\n14\tline14\n15\tline15\n16\tline16\n17\tline17\n18\tline18\n19\tline19"
	assert.Equal(t, want, got.Content)

	// The final window has no more lines → not truncated.
	res, err = ReadFile(root, "big.txt", 45, 10)
	require.NoError(t, err)
	got = res.(readResult)
	assert.False(t, got.Truncated)
	assert.Equal(t, 45, got.StartLine)
}

func TestRead_DefaultLimit_TruncatesLargeFile(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 1; i <= defaultReadLimit+100; i++ { // 2100 lines, > the 2000 default
		lines = append(lines, "x")
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "huge.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	res, err := ReadFile(root, "huge.txt", 0, 0) // defaults: offset=1, limit=2000
	require.NoError(t, err)
	got := res.(readResult)
	assert.Equal(t, defaultReadLimit+100, got.TotalLines)
	assert.True(t, got.Truncated)
	assert.Equal(t, defaultReadLimit, strings.Count(got.Content, "\n")+1, "returned exactly the default limit window")
}

func TestRead_BinaryRejected(t *testing.T) {
	root := t.TempDir()
	bin := append([]byte("hello"), 0, 0, 0) // NUL bytes -> binary
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin.dat"), bin, 0o644))

	_, err := ReadFile(root, "bin.dat", 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary")
}

func TestRead_NonExistentFile_ErrFileNotFound(t *testing.T) {
	root := t.TempDir()
	_, err := ReadFile(root, "nope.txt", 0, 0)
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
	if !IsFileNotFound(err) {
		t.Fatal("IsFileNotFound returned false on ErrFileNotFound")
	}
}

func TestRead_OutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := ReadFile(root, "../../etc/passwd", 0, 0)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}
