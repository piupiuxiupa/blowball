package xizhi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunSearch_CtxCancelled verifies the execution half honors ctx
// cancellation for whichever engine is resolved: fd aborts via
// exec.CommandContext (Start returns the ctx error on an already-done ctx) and
// the Go walker short-circuits on its per-entry ctx.Err() check. RunSearch is
// what the workspace search REST handler drives under a deadline, so a stalled
// walk must surface the ctx error rather than hang or swallow it.
func TestRunSearch_CtxCancelled(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "deep", "nest"), 0o755))
	for i := range 50 {
		require.NoError(t, os.WriteFile(
			filepath.Join(root, "deep", "nest", fmt.Sprintf("f%02d.txt", i)),
			[]byte("x"), 0o644))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ps := PreparedSearch{
		RelPath:   ".",
		AbsPath:   root,
		EntryType: findTypeAny,
		HeadLimit: defaultFindHeadLimit,
	}
	page, err := RunSearch(ctx, ps)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "err = %v, want context.Canceled", err)
	assert.Empty(t, page.Entries)
}
