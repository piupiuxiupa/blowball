// Package fs provides the filesystem tier of the blowball per-user data tree.
//
// Each user owns a directory under root named after their user_id; inside it
// the workspace/ subdirectory holds the user's files (the xizhi_* tools and
// the workspace REST API are scoped here) plus the reserved
// .blowball/skills/ namespace for per-user skills.
//
// Historically this package also held the session-file warm tier
// ({root}/{userID}/sessions/{sessionID}.json) of the three-layer message
// store; the Redis-first write-behind persistence change removed that tier
// entirely (messages now flow Redis ingest queue → MySQL flusher, see the
// message-write-behind capability). Legacy sessions/ directories left on
// disk are never read or written again; operators may clean them up at will.
package fs

import (
	"context"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// mkdirAll creates dir (and any missing parents) with mode 0o755. It is a
// thin wrapper over os.MkdirAll so callers across this package stay short.
func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// dirExists reports whether dir exists and is a directory. A symlink to a
// directory is reported as a directory.
func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// Store wraps a root directory. The directory layout under root is:
//
//	{root}/{userID}/workspace/...                        (user files; xizhi_* scoped here)
//	{root}/{userID}/workspace/.blowball/skills/...       (reserved; per-user skills)
type Store struct {
	root string
}

// New returns a Store rooted at root, creating root if it does not already
// exist. The returned Store is safe for concurrent use because the underlying
// filesystem calls are themselves safe.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("fs store: root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fs store: mkdir root %q: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Root returns the configured root directory.
func (s *Store) Root() string { return s.root }

// logFS emits a debug log describing the about-to-be-run fs operation. It
// mirrors logQuery / logCmd in the other store packages.
func logFS(ctx context.Context, op, path string) {
	fields := []zap.Field{
		zap.String("op", op),
		zap.String("path", path),
	}
	if tid := trace.FromContext(ctx); tid != "" {
		fields = append(fields, zap.String("trace_id", tid))
	}
	logger.L().Debug("fs operation", fields...)
}
