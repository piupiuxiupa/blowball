// Package resource owns the user-scoped `.cron/<task_id>` resource namespace:
// controlled initialization, draft revisions, fenced immutable publication,
// pinned execution validation and the write-protection guard shared by every
// workspace/file entry point.
package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Namespace constants.
const (
	CronDirName     = ".cron"
	ManagedMarker   = ".blowball-managed.json"
	TaskMarker      = ".task.json"
	DraftDir        = "draft"
	RevisionsDir    = "revisions"
	StagingDir      = ".staging"
	ManifestFile    = "manifest.json"
	MaxRelativeName = 512
)

var (
	// ErrNamespaceConflict reports existing unmanaged `.cron` content.
	ErrNamespaceConflict = errors.New("cron resource namespace conflict")
	// ErrReadOnly reports a write to a protected resource path.
	ErrReadOnly = errors.New("cron resource path is read-only")
	// ErrNotFound reports a missing task/resource/version.
	ErrNotFound = errors.New("cron resource not found")
	// ErrUnavailable reports a dependency (Scheduler/MySQL) failure; the
	// caller must fail closed with 503 — never fall back to the filesystem.
	ErrUnavailable = errors.New("cron resource dependency unavailable")
	// ErrDraftConflict reports a concurrent draft change during publication.
	ErrDraftConflict = errors.New("cron resource draft changed during publication")
	// ErrLimit reports publication limits.
	ErrLimit = errors.New("cron resource limit exceeded")
	// ErrInvalidPath reports an unsafe resource path.
	ErrInvalidPath = errors.New("invalid cron resource path")
	// ErrInvalidRequest reports missing/invalid publication input.
	ErrInvalidRequest = errors.New("invalid cron resource request")
)

// Limits bounds one publication.
type Limits struct {
	MaxFiles        int
	MaxTotalBytes   int64
	MaxFileBytes    int64
	MaxContextFiles int
	MaxContextBytes int
}

// DefaultLimits mirrors config.ScheduledResourcesConfig defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles:        1000,
		MaxTotalBytes:   100 << 20,
		MaxFileBytes:    100 << 20,
		MaxContextFiles: 100,
		MaxContextBytes: 4000,
	}
}

// FileEntry is one manifest entry.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the immutable revision manifest stored inside the revision dir.
type Manifest struct {
	ResourceRevision uint64      `json:"resource_revision"`
	ManifestHash     string      `json:"manifest_hash"`
	Files            []FileEntry `json:"files"`
}

// ManifestHash computes the canonical digest over sorted entries. The same
// function is used at publication and execution validation.
func ManifestHash(files []FileEntry) string {
	sorted := make([]FileEntry, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", f.Path, f.Size, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BuildStaging copies the draft into stagingDir, computes the manifest from the
// ACTUAL copied bytes, writes manifest.json, and returns the manifest. Only
// regular files are accepted; symlinks, devices, FIFOs and escaping paths are
// rejected. An empty draft produces an empty manifest.
func BuildStaging(draftRoot, stagingDir string, revision uint64, limits Limits) (*Manifest, error) {
	info, err := os.Lstat(draftRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: draft is missing", ErrNotFound)
		}
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: draft is not a directory", ErrNamespaceConflict)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	var files []FileEntry
	var total int64
	err = filepath.WalkDir(draftRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == draftRoot {
			return nil
		}
		rel, err := filepath.Rel(draftRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "" || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) || len(rel) > MaxRelativeName {
			return fmt.Errorf("%w: unsafe entry %q", ErrInvalidPath, rel)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink %q", ErrInvalidPath, rel)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: non-regular file %q", ErrInvalidPath, rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > limits.MaxFileBytes {
			return fmt.Errorf("%w: file %q exceeds max_file_bytes", ErrLimit, rel)
		}
		if len(files)+1 > limits.MaxFiles {
			return fmt.Errorf("%w: max_files exceeded", ErrLimit)
		}
		total += fi.Size()
		if total > limits.MaxTotalBytes {
			return fmt.Errorf("%w: max_total_bytes exceeded", ErrLimit)
		}
		dst := filepath.Join(stagingDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		sum, err := copyFile(dst, p)
		if err != nil {
			return err
		}
		files = append(files, FileEntry{Path: rel, Size: fi.Size(), SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m := &Manifest{ResourceRevision: revision, ManifestHash: ManifestHash(files), Files: files}
	if err := writeManifest(filepath.Join(stagingDir, ManifestFile), m); err != nil {
		return nil, err
	}
	return m, nil
}

func copyFile(dst, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadManifest parses a revision's manifest.
func ReadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeManifest(path string, m *Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// VerifyPinned checks the revision directory against the stored manifest hash:
// the manifest itself, every listed file's size and SHA-256, and the absence of
// extra files. It returns the parsed manifest.
func VerifyPinned(revisionRoot, expectedHash string) (*Manifest, error) {
	m, err := ReadManifest(filepath.Join(revisionRoot, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", ErrNotFound, err)
	}
	if m.ManifestHash != expectedHash || ManifestHash(m.Files) != expectedHash {
		return nil, fmt.Errorf("%w: manifest hash mismatch", ErrDraftConflict)
	}
	listed := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		listed[f.Path] = struct{}{}
		p := filepath.Join(revisionRoot, filepath.FromSlash(f.Path))
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("%w: file %q missing", ErrNotFound, f.Path)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: file %q is not regular", ErrInvalidPath, f.Path)
		}
		if fi.Size() != f.Size {
			return nil, fmt.Errorf("%w: file %q size mismatch", ErrDraftConflict, f.Path)
		}
		sum, err := hashFile(p)
		if err != nil {
			return nil, err
		}
		if sum != f.SHA256 {
			return nil, fmt.Errorf("%w: file %q digest mismatch", ErrDraftConflict, f.Path)
		}
	}
	err = filepath.WalkDir(revisionRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == revisionRoot || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(revisionRoot, p)
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile {
			return nil
		}
		if _, ok := listed[rel]; !ok {
			return fmt.Errorf("%w: unlisted file %q", ErrInvalidPath, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ListDraft returns the draft files (path/size) without hashing.
func ListDraft(draftRoot string) ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(draftRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == draftRoot || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("%w: non-regular draft entry", ErrInvalidPath)
		}
		rel, _ := filepath.Rel(draftRoot, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: filepath.ToSlash(rel), Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
