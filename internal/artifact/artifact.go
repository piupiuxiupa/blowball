// Package artifact implements the turn-artifacts capability: at the end of
// each turn it detects the files the turn produced (deliverables written
// anywhere in the workspace by ANY tool path — xizhi_* or the bash sandbox),
// snapshots their content into a server-side version store, and records the
// version in a MySQL index so historical references can always open the
// content as it was at that turn's end.
//
// Detection is mtime-based (files modified since turn start) and excludes the
// scratch area (tmp/), the reserved .blowball/ namespace, and hidden entries
// at any depth — mirroring the prompt's deliverable-vs-scratch convention.
// Snapshots live outside the user workspace, so neither xizhi_* nor the bash
// sandbox can reach the version store.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
)

// Artifact op values recorded on artifact stream events.
const (
	// OpCreate marks a path with no prior version record.
	OpCreate = "create"
	// OpUpdate marks a path that already had at least one version record.
	OpUpdate = "update"
)

// Artifact is one detected turn deliverable. VersionID is empty when the
// file was detected but not snapshotted (over the size cap, or a read/store
// failure) — the artifact is still announced so the frontend can open the
// current file.
type Artifact struct {
	Path      string `json:"path"`
	VersionID string `json:"version_id,omitempty"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime"`
	Op        string `json:"op"`
}

// Index is the MySQL-backed metadata side of the version store. The blob
// bytes live on disk (BlobStore); Index answers ownership, dedup and as-of
// queries.
type Index interface {
	// InsertVersion records one snapshot. Callers mint a fresh UUID v7 per
	// record, so collisions are not a dedup signal.
	InsertVersion(ctx context.Context, rec model.FileVersion) error
	// LatestVersion returns the newest record for (userID, path), or
	// (nil, nil) when the path was never versioned.
	LatestVersion(ctx context.Context, userID, path string) (*model.FileVersion, error)
	// VersionAsOf returns the newest record for (userID, path) created at or
	// before before, or (nil, nil) when none exists.
	VersionAsOf(ctx context.Context, userID, path string, before time.Time) (*model.FileVersion, error)
	// VersionByID returns the record for versionID, or (nil, nil) when it
	// does not exist.
	VersionByID(ctx context.Context, versionID string) (*model.FileVersion, error)
}

// Service ties the blob store and index together and exposes the turn-end
// finalization plus the read paths behind the HTTP endpoints.
type Service struct {
	blobs    *BlobStore
	index    Index
	maxBytes int64
}

// NewService wires the artifact service. maxBytes <= 0 disables the size cap
// (not recommended; the config layer defaults it to 200MB).
func NewService(blobs *BlobStore, index Index, maxBytes int64) *Service {
	return &Service{blobs: blobs, index: index, maxBytes: maxBytes}
}

// Detect returns the workspace-relative slash-separated paths of regular,
// non-empty files whose mtime is at or after since, excluding tmp/, hidden
// entries (any path segment starting with "."), and symlinks. The result is
// sorted (filepath.WalkDir order) so callers get deterministic output.
func Detect(wsRoot string, since time.Time) ([]string, error) {
	var out []string
	err := filepath.WalkDir(wsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entries (permissions, dangling symlinks) are skipped,
			// not fatal: detection is best-effort at turn end.
			return nil
		}
		rel, err := filepath.Rel(wsRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (rel == "tmp" || strings.HasPrefix(rel, "tmp/") || isHiddenPath(rel)) {
				return fs.SkipDir
			}
			return nil
		}
		if isHiddenPath(rel) || strings.HasPrefix(rel, "tmp/") {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.ModTime().Before(since) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out, err
}

// isHiddenPath reports whether any segment of a slash-separated relative path
// starts with "." (covering .blowball/, .pip/, .git/ and dotfiles at any
// depth). The root "." itself is not hidden.
func isHiddenPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	return false
}

// FinalizeTurn detects the turn's artifacts (files with mtime >= since) and
// snapshots each into the version store, returning the artifacts in path
// order. Per-file failures (read/store/index errors, over-cap files) degrade
// to an artifact without a VersionID rather than failing the turn; ctx
// cancellation stops the loop early.
func (s *Service) FinalizeTurn(ctx context.Context, userID, wsRoot string, since time.Time) ([]Artifact, error) {
	paths, err := Detect(wsRoot, since)
	if err != nil {
		return nil, fmt.Errorf("artifact: detect in %q: %w", wsRoot, err)
	}
	arts := make([]Artifact, 0, len(paths))
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return arts, err
		}
		arts = append(arts, s.snapshotOne(ctx, userID, wsRoot, rel))
	}
	return arts, nil
}

// snapshotOne snapshots one detected file. The returned Artifact always
// carries Path/Size/Mime/Op; VersionID is set only when the snapshot (or a
// dedup hit) succeeded.
func (s *Service) snapshotOne(ctx context.Context, userID, wsRoot, rel string) Artifact {
	abs := filepath.Join(wsRoot, filepath.FromSlash(rel))
	a := Artifact{Path: rel, Op: OpCreate}

	info, err := os.Stat(abs)
	if err != nil {
		return a // vanished between Detect and stat: announce with zero size
	}
	a.Size = info.Size()
	a.Mime = mimeOf(rel, abs)

	// Latest doubles as the op signal (nil -> create) and the dedup source.
	// Index failures degrade to "no snapshot" below; the artifact is still
	// announced so the file stays reachable as its current version.
	latest, lerr := s.index.LatestVersion(ctx, userID, rel)
	if lerr == nil && latest != nil {
		a.Op = OpUpdate
	}

	if s.maxBytes > 0 && a.Size > s.maxBytes {
		return a
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return a
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	if lerr != nil {
		return a
	}
	if latest != nil && latest.SHA256 == digest {
		// Byte-identical to the latest version (mtime touched only): reuse
		// the existing version_id instead of minting a duplicate snapshot.
		a.VersionID = latest.VersionID
		return a
	}

	vid, err := uuid.NewV7()
	if err != nil {
		return a
	}
	rec := model.FileVersion{
		UserID:    userID,
		Path:      rel,
		VersionID: vid.String(),
		Size:      a.Size,
		Mime:      a.Mime,
		SHA256:    digest,
	}
	if err := s.blobs.Write(userID, rel, rec.VersionID, data); err != nil {
		return a
	}
	if err := s.index.InsertVersion(ctx, rec); err != nil {
		// The blob is orphaned (harmless); without the index row the version
		// is unreachable, so report the artifact unsnapshotted.
		return a
	}
	a.VersionID = rec.VersionID
	return a
}

// Resolve returns the newest version record for (userID, path). When before
// is non-zero it bounds the lookup to snapshots created at or before that
// time (cross-turn "as of that message" resolution). (nil, nil) means the
// path was never versioned.
func (s *Service) Resolve(ctx context.Context, userID, path string, before time.Time) (*model.FileVersion, error) {
	if before.IsZero() {
		return s.index.LatestVersion(ctx, userID, path)
	}
	return s.index.VersionAsOf(ctx, userID, path, before)
}

// GetVersionForUser returns the version record when it exists AND belongs to
// userID, without reading the blob — the ownership check behind endpoints
// that embed the version id (e.g. the OnlyOffice version config) rather than
// serve bytes. (nil, nil) means unknown or cross-user.
func (s *Service) GetVersionForUser(ctx context.Context, userID, versionID string) (*model.FileVersion, error) {
	rec, err := s.index.VersionByID(ctx, versionID)
	if err != nil || rec == nil || rec.UserID != userID {
		return nil, err
	}
	return rec, nil
}

// OpenVersion returns the record and content bytes for versionID. Ownership
// is enforced here: a version belonging to another user returns (nil, nil,
// nil) so callers map it to 404 without revealing existence.
func (s *Service) OpenVersion(ctx context.Context, userID, versionID string) (*model.FileVersion, []byte, error) {
	rec, err := s.index.VersionByID(ctx, versionID)
	if err != nil || rec == nil || rec.UserID != userID {
		return nil, nil, err
	}
	data, err := s.blobs.Read(rec.UserID, rec.Path, rec.VersionID)
	if err != nil {
		return nil, nil, fmt.Errorf("artifact: read version blob %q: %w", versionID, err)
	}
	return rec, data, nil
}

// mimeOf infers the content type: extension first, then a 512-byte sniff for
// extension-less or unknown names, finally octet-stream.
func mimeOf(rel, abs string) string {
	if ext := filepath.Ext(rel); ext != "" {
		if mt := mime.TypeByExtension(strings.ToLower(ext)); mt != "" {
			return mt
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	var head [512]byte
	n, _ := f.Read(head[:])
	if n == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head[:n])
}
