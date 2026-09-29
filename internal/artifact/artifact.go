// Package artifact implements the turn-artifacts capability: at the end of
// each turn it reads the deliverables explicitly linked in the assistant
// reply, snapshots their content into the office-vers version store, and
// records the version in a MySQL index so historical references can always
// open the content as it was at that turn's end.
//
// A blowball://workspace link is the sole attribution signal. The scratch area
// (tmp/), the reserved .blowball/ namespace, and hidden entries at any depth
// remain excluded. Snapshots live in office-vers (outside the user workspace),
// so neither xizhi_* nor the bash sandbox can reach the version store.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"

	"go.uber.org/zap"
)

var workspaceLinkRE = regexp.MustCompile(`\[[^\]\n]*\]\((blowball://workspace/[^)\n]+)\)`)

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

// Service ties the version store and index together and exposes the turn-end
// finalization plus the read paths behind the HTTP endpoints.
type Service struct {
	store    VersionStore
	index    Index
	maxBytes int64
}

// NewService wires the artifact service. A nil store disables snapshotting
// (office-vers unconfigured): artifacts are still detected and announced,
// just without a version_id. maxBytes <= 0 disables the size cap (not
// recommended; the config layer defaults it to 200MB).
func NewService(store VersionStore, index Index, maxBytes int64) *Service {
	return &Service{store: store, index: index, maxBytes: maxBytes}
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

// declaredPaths extracts workspace-relative paths from the assistant reply's
// markdown artifact links. Invalid links are skipped with a WARN; valid paths
// are deduplicated and sorted so artifact events are deterministic.
func declaredPaths(ctx context.Context, userID, content string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, match := range workspaceLinkRE.FindAllStringSubmatch(content, -1) {
		rel, err := workspacePath(match[1])
		if err != nil {
			warnSkip(ctx, "invalid artifact link", match[1], userID, err)
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// workspacePath validates and normalizes one blowball://workspace URL. It
// returns the decoded slash-separated workspace-relative path.
func workspacePath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "blowball" || u.Host != "workspace" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("link must be blowball://workspace/<relative-path> without query or fragment")
	}

	rel := strings.TrimPrefix(u.Path, "/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path traversal is not allowed")
		}
	}
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") ||
		isHiddenPath(rel) || rel == "tmp" || strings.HasPrefix(rel, "tmp/") {
		return "", fmt.Errorf("path must be a visible workspace-relative file path")
	}
	rel = path.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	return rel, nil
}

// FinalizeTurn snapshots each artifact explicitly linked in assistantContent,
// returning the artifacts in path order. Invalid links and unusable targets
// are skipped. Other per-file failures (read/store/index errors, over-cap
// files) degrade to an artifact without a VersionID rather than failing the
// turn; ctx cancellation stops the loop early.
func (s *Service) FinalizeTurn(ctx context.Context, userID, wsRoot, assistantContent string) ([]Artifact, error) {
	paths := declaredPaths(ctx, userID, assistantContent)
	arts := make([]Artifact, 0, len(paths))
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return arts, err
		}
		if a, ok := s.snapshotOne(ctx, userID, wsRoot, rel); ok {
			arts = append(arts, a)
		}
	}
	return arts, nil
}

// snapshotOne snapshots one declared file. The returned Artifact always
// carries Path/Size/Mime/Op; VersionID is set only when the snapshot (or a
// dedup hit) succeeded. ok=false means the target is not an existing regular,
// non-empty file and must not be announced.
func (s *Service) snapshotOne(ctx context.Context, userID, wsRoot, rel string) (Artifact, bool) {
	abs := filepath.Join(wsRoot, filepath.FromSlash(rel))
	a := Artifact{Path: rel, Op: OpCreate}

	info, err := os.Lstat(abs)
	if err != nil {
		warnSkip(ctx, "target unavailable", rel, userID, err)
		return Artifact{}, false
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		warnSkip(ctx, "target is not a regular non-empty file", rel, userID, nil)
		return Artifact{}, false
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

	if s.store == nil {
		return a, true // version store unconfigured: announce without snapshot
	}
	if s.maxBytes > 0 && a.Size > s.maxBytes {
		warnSkip(ctx, "over size cap", rel, userID, nil)
		return a, true
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		warnSkip(ctx, "read failed", rel, userID, err)
		return a, true
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	if lerr != nil {
		return a, true
	}
	if latest != nil && latest.SHA256 == digest {
		// Byte-identical to the latest version (mtime touched only): reuse
		// the existing version_id instead of storing a duplicate snapshot
		// (office-vers creates a new version per POST — dedup is our job).
		a.VersionID = latest.VersionID
		return a, true
	}

	vid, err := s.store.Put(ctx, userID, rel, data)
	if err != nil {
		warnSkip(ctx, "version store put failed", rel, userID, err)
		return a, true
	}
	rec := model.FileVersion{
		UserID:    userID,
		Path:      rel,
		VersionID: vid,
		Size:      a.Size,
		Mime:      a.Mime,
		SHA256:    digest,
	}
	if err := s.index.InsertVersion(ctx, rec); err != nil {
		// The version is orphaned in office-vers (harmless); without the
		// index row it is unreachable, so report the artifact unsnapshotted.
		warnSkip(ctx, "version index insert failed", rel, userID, err)
		return a, true
	}
	a.VersionID = rec.VersionID
	return a, true
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

// OpenVersion returns the record and content bytes for versionID. Ownership
// is enforced here: a version belonging to another user returns (nil, nil,
// nil) so callers map it to 404 without revealing existence.
func (s *Service) OpenVersion(ctx context.Context, userID, versionID string) (*model.FileVersion, []byte, error) {
	rec, err := s.index.VersionByID(ctx, versionID)
	if err != nil || rec == nil || rec.UserID != userID {
		return nil, nil, err
	}
	if s.store == nil {
		return nil, nil, fmt.Errorf("artifact: version store not configured")
	}
	data, err := s.store.Get(ctx, rec.UserID, rec.Path, rec.VersionID)
	if err != nil {
		return nil, nil, fmt.Errorf("artifact: read version %q: %w", versionID, err)
	}
	return rec, data, nil
}

// warnSkip logs a skipped link/target or snapshot degradation. The turn still
// completes; only the affected artifact loses its version or announcement.
func warnSkip(ctx context.Context, reason, rel, userID string, err error) {
	logger.FromContext(ctx).Warn("artifact snapshot skipped",
		zap.String("op", "artifact.snapshot"),
		zap.String("reason", reason),
		zap.String("path", rel),
		zap.String("user_id", userID),
		zap.Error(err))
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
