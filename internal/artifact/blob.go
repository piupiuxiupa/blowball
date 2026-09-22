package artifact

import (
	"fmt"
	"os"
	"path/filepath"
)

// BlobStore is the content half of the version store: one file per snapshot
// under {root}/{userID}/{path}.versions/{versionID}. The ".versions" suffix
// keeps version directories disjoint from any workspace path shape, and the
// whole tree lives outside every user's workspace so xizhi_*/bash cannot
// reach it.
type BlobStore struct {
	root string
}

// NewBlobStore returns a store rooted at root, creating it if missing.
func NewBlobStore(root string) (*BlobStore, error) {
	if root == "" {
		return nil, fmt.Errorf("artifact blob store: root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("artifact blob store: mkdir root %q: %w", root, err)
	}
	return &BlobStore{root: root}, nil
}

// blobPath resolves the snapshot file location. rel is a workspace-relative
// slash path produced by Detect (never user input on the read path: reads key
// off the MySQL index record), so traversal is not a concern here.
func (s *BlobStore) blobPath(userID, rel, versionID string) string {
	return filepath.Join(s.root, userID, filepath.FromSlash(rel)+".versions", versionID)
}

// Write stores one snapshot blob, creating parent directories as needed.
func (s *BlobStore) Write(userID, rel, versionID string, data []byte) error {
	p := s.blobPath(userID, rel, versionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("artifact blob store: mkdir for %q: %w", p, err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("artifact blob store: write %q: %w", p, err)
	}
	return nil
}

// Read returns the snapshot bytes.
func (s *BlobStore) Read(userID, rel, versionID string) ([]byte, error) {
	return os.ReadFile(s.blobPath(userID, rel, versionID))
}
