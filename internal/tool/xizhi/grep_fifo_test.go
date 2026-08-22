//go:build !windows

package xizhi

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrepFiles_SpecialFileRejected covers a path that is neither a directory
// nor a regular file (a named pipe): xizhi_grep must refuse it with an explicit
// error instead of searching.
func TestGrepFiles_SpecialFileRejected(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o644))

	fi, err := os.Stat(fifo)
	require.NoError(t, err)
	require.False(t, fi.IsDir())
	require.False(t, fi.Mode().IsRegular())

	_, err = GrepFiles(root, "pipe", "x", "", false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a file or directory")
}
