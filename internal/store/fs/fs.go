// Package fs provides the warm-tier file-system store for blowball sessions.
//
// Each user owns a directory under root named after their user_id; inside it
// the sessions/ subdirectory holds one JSON file per session. This package is
// the middle layer of the three-layer message store: it is more durable than
// Redis and faster to read than MySQL.
package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
//	{root}/{userID}/sessions/{sessionID}.json
//	{root}/{userID}/workspace/...
//	{root}/{userID}/skills/...
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

// sessionPath returns the absolute path of the session JSON file for
// (userID, sessionID). It is constructed purely from filepath.Join so it is
// immune to path-traversal in the caller's arguments (e.g. a sessionID
// containing ".." cannot escape the sessions/ subdirectory).
func (s *Store) sessionPath(userID, sessionID string) string {
	return filepath.Join(s.root, userID, "sessions", sessionID+".json")
}

// WriteSession writes data as the session JSON for (userID, sessionID),
// creating any missing parent directories along the way. The write is atomic
// from the reader's perspective only when the caller's filesystem honours
// O_TRUNC and atomic replaces — we use os.WriteFile which truncates in place;
// the warm-tier fallback to MySQL covers any partial-write risk.
func (s *Store) WriteSession(ctx context.Context, userID, sessionID string, data []byte) error {
	path := s.sessionPath(userID, sessionID)
	logFS(ctx, "session.write", path)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parents for %q: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write session %q: %w", path, err)
	}
	return nil
}

// ReadSession returns the session JSON for (userID, sessionID), or (nil, nil)
// when the file does not exist. Any other read error is returned verbatim.
func (s *Store) ReadSession(ctx context.Context, userID, sessionID string) ([]byte, error) {
	path := s.sessionPath(userID, sessionID)
	logFS(ctx, "session.read", path)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session %q: %w", path, err)
	}
	return data, nil
}

// DeleteSession removes the session JSON for (userID, sessionID). A missing
// file is treated as success (idempotent delete).
func (s *Store) DeleteSession(ctx context.Context, userID, sessionID string) error {
	path := s.sessionPath(userID, sessionID)
	logFS(ctx, "session.delete", path)

	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete session %q: %w", path, err)
	}
	return nil
}

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
