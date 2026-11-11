package resource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lush/blowball/internal/model"
)

// TaskOwner is the Scheduler-backed ownership check. It must fail closed when
// the Scheduler is unavailable.
type TaskOwner interface {
	OwnsTask(ctx context.Context, userID, taskID string) (bool, error)
}

// ReferenceChecker answers the maintenance reference check.
type ReferenceChecker interface {
	ResourceReferenced(ctx context.Context, taskID string, revision uint64) (bool, error)
}

// Store is the main-service resource metadata surface.
type Store interface {
	EnsureResourceDraft(ctx context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error)
	GetResourceDraft(ctx context.Context, userID, taskID string) (*model.ScheduledResourceDraft, error)
	BumpResourceDraft(ctx context.Context, userID, taskID string) (uint64, error)
	NextResourceRevision(ctx context.Context, taskID string) (uint64, error)
	FindResourceVersionByRequest(ctx context.Context, userID, taskID, requestID string) (*model.ScheduledResourceVersion, error)
	InsertResourceVersion(ctx context.Context, v model.ScheduledResourceVersion) (bool, error)
	GetResourceVersion(ctx context.Context, userID, taskID string, revision uint64) (*model.ScheduledResourceVersion, error)
	ListResourceVersions(ctx context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error)
	DeleteResourceVersion(ctx context.Context, userID, taskID string, revision uint64) (bool, error)
	WithinTaskResourceLock(ctx context.Context, taskID string, fn func() error) error
}

// DraftInfo is the resource GET DTO.
type DraftInfo struct {
	TaskID        string      `json:"task_id"`
	DraftRevision uint64      `json:"draft_revision"`
	Files         []FileEntry `json:"files"`
}

// VersionInfo is the published-version GET DTO.
type VersionInfo struct {
	TaskID       string      `json:"task_id"`
	Revision     uint64      `json:"resource_revision"`
	ManifestHash string      `json:"manifest_hash"`
	FileCount    int         `json:"file_count"`
	TotalBytes   int64       `json:"total_bytes"`
	Files        []FileEntry `json:"files"`
	CreateTime   time.Time   `json:"create_time"`
}

// Service implements the controlled `.cron` resource lifecycle.
type Service struct {
	store     Store
	tasks     TaskOwner
	workspace func(userID string) string
	limits    Limits
}

// New wires the resource service.
func New(store Store, tasks TaskOwner, workspace func(userID string) string, limits Limits) *Service {
	if limits.MaxFiles <= 0 {
		limits = DefaultLimits()
	}
	return &Service{store: store, tasks: tasks, workspace: workspace, limits: limits}
}

// Limits returns the effective publication/context limits.
func (s *Service) Limits() Limits { return s.limits }

// WorkspaceRoot resolves the user's workspace root (server-side only).
func (s *Service) WorkspaceRoot(userID string) string { return s.workspace(userID) }

// EnsureDraft verifies ownership and performs controlled namespace/draft
// initialization.
func (s *Service) EnsureDraft(ctx context.Context, userID, taskID string) (Layout, error) {
	if err := s.requireOwner(ctx, userID, taskID); err != nil {
		return Layout{}, err
	}
	layout, err := EnsureLayout(ctx, s.workspace(userID), userID, taskID)
	if err != nil {
		return Layout{}, err
	}
	if _, err := s.store.EnsureResourceDraft(ctx, userID, taskID); err != nil {
		return Layout{}, fmt.Errorf("%w: draft metadata: %v", ErrUnavailable, err)
	}
	return layout, nil
}

// BumpDraft advances the draft fence after an authorized write.
func (s *Service) BumpDraft(ctx context.Context, userID, taskID string) error {
	layout, err := s.EnsureDraft(ctx, userID, taskID)
	if err != nil {
		return err
	}
	_ = layout
	if _, err := s.store.BumpResourceDraft(ctx, userID, taskID); err != nil {
		return fmt.Errorf("%w: draft bump: %v", ErrUnavailable, err)
	}
	return nil
}

// GetDraft returns the draft revision and file list. A task with no resources
// returns revision 0 and an empty list without initializing anything.
func (s *Service) GetDraft(ctx context.Context, userID, taskID string) (*DraftInfo, error) {
	if err := s.requireOwner(ctx, userID, taskID); err != nil {
		return nil, err
	}
	layout, err := NewLayout(s.workspace(userID), taskID)
	if err != nil {
		return nil, err
	}
	d, err := s.store.GetResourceDraft(ctx, userID, taskID)
	if err != nil {
		return nil, fmt.Errorf("%w: draft metadata: %v", ErrUnavailable, err)
	}
	info := &DraftInfo{TaskID: taskID, Files: []FileEntry{}}
	if d != nil {
		info.DraftRevision = d.DraftRevision
	}
	files, err := ListDraft(layout.DraftRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return info, nil
		}
		return nil, err
	}
	info.Files = files
	return info, nil
}

// Publish materializes the current draft into a new immutable revision.
func (s *Service) Publish(ctx context.Context, userID, taskID string, expectedDraftRevision uint64, requestID string) (*model.ScheduledResourceVersion, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 64 {
		return nil, fmt.Errorf("%w: request_id must be non-empty and at most 64 characters", ErrInvalidRequest)
	}
	layout, err := s.EnsureDraft(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.FindResourceVersionByRequest(ctx, userID, taskID, requestID)
	if err != nil {
		return nil, fmt.Errorf("%w: publication lookup: %v", ErrUnavailable, err)
	}
	if existing != nil {
		return existing, nil
	}

	var out *model.ScheduledResourceVersion
	err = s.store.WithinTaskResourceLock(ctx, taskID, func() error {
		draft, err := s.store.GetResourceDraft(ctx, userID, taskID)
		if err != nil {
			return fmt.Errorf("%w: draft metadata: %v", ErrUnavailable, err)
		}
		if draft == nil {
			return ErrNotFound
		}
		if expectedDraftRevision > 0 && draft.DraftRevision != expectedDraftRevision {
			return ErrDraftConflict
		}
		revision, err := s.allocateRevision(ctx, userID, layout)
		if err != nil {
			return err
		}
		staging := filepath.Join(layout.StagingRoot(), requestID+"-"+randomSuffix())
		manifest, err := BuildStaging(layout.DraftRoot(), staging, revision, s.limits)
		if err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
		// Fence: a draft mutation during the copy invalidates this attempt.
		after, err := s.store.GetResourceDraft(ctx, userID, taskID)
		if err != nil || after == nil || after.DraftRevision != draft.DraftRevision || after.Generation != draft.Generation {
			_ = os.RemoveAll(staging)
			return ErrDraftConflict
		}
		final := layout.RevisionRoot(revision)
		if _, err := os.Lstat(final); err == nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("%w: revision directory already exists", ErrDraftConflict)
		} else if !os.IsNotExist(err) {
			_ = os.RemoveAll(staging)
			return err
		}
		if err := os.Rename(staging, final); err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("%w: publish rename: %v", ErrUnavailable, err)
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			_ = os.RemoveAll(final)
			return err
		}
		v := model.ScheduledResourceVersion{
			TaskID: taskID, Revision: revision, UserID: userID,
			ManifestHash: manifest.ManifestHash, FileCount: len(manifest.Files),
			TotalBytes: totalSize(manifest.Files), ManifestJSON: string(raw), RequestID: requestID,
		}
		inserted, err := s.store.InsertResourceVersion(ctx, v)
		if err != nil {
			_ = os.RemoveAll(final)
			return fmt.Errorf("%w: version metadata: %v", ErrUnavailable, err)
		}
		if !inserted {
			_ = os.RemoveAll(final)
			existing, ferr := s.store.FindResourceVersionByRequest(ctx, userID, taskID, requestID)
			if ferr != nil {
				return fmt.Errorf("%w: publication re-read: %v", ErrUnavailable, ferr)
			}
			if existing == nil {
				return ErrDraftConflict
			}
			out = existing
			return nil
		}
		out = &v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) allocateRevision(ctx context.Context, userID string, layout Layout) (uint64, error) {
	revision, err := s.store.NextResourceRevision(ctx, layout.TaskID)
	if err != nil {
		return 0, fmt.Errorf("%w: revision allocation: %v", ErrUnavailable, err)
	}
	for i := 0; i < 100; i++ {
		_, statErr := os.Lstat(layout.RevisionRoot(revision))
		existing, metaErr := s.store.GetResourceVersion(ctx, userID, layout.TaskID, revision)
		if metaErr != nil {
			return 0, fmt.Errorf("%w: revision lookup: %v", ErrUnavailable, metaErr)
		}
		if os.IsNotExist(statErr) && existing == nil {
			return revision, nil
		}
		revision++
	}
	return 0, fmt.Errorf("%w: no free revision after 100 attempts", ErrUnavailable)
}

// GetVersion returns one owner/task-scoped published version.
func (s *Service) GetVersion(ctx context.Context, userID, taskID string, revision uint64) (*VersionInfo, error) {
	if err := s.requireOwner(ctx, userID, taskID); err != nil {
		return nil, err
	}
	v, err := s.store.GetResourceVersion(ctx, userID, taskID, revision)
	if err != nil {
		return nil, fmt.Errorf("%w: version lookup: %v", ErrUnavailable, err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return versionInfo(v), nil
}

// ListVersions returns the task's published version metadata, newest first.
func (s *Service) ListVersions(ctx context.Context, userID, taskID string) ([]model.ScheduledResourceVersion, error) {
	if err := s.requireOwner(ctx, userID, taskID); err != nil {
		return nil, err
	}
	versions, err := s.store.ListResourceVersions(ctx, userID, taskID)
	if err != nil {
		return nil, fmt.Errorf("%w: version list: %v", ErrUnavailable, err)
	}
	return versions, nil
}

// ValidateBinding checks that the revision exists for this owner/task and the
// manifest hash matches. It is the Task configuration gate.
func (s *Service) ValidateBinding(ctx context.Context, userID, taskID string, revision uint64, manifestHash string) error {
	v, err := s.store.GetResourceVersion(ctx, userID, taskID, revision)
	if err != nil {
		return fmt.Errorf("%w: version lookup: %v", ErrUnavailable, err)
	}
	if v == nil || v.ManifestHash != strings.TrimSpace(manifestHash) {
		return fmt.Errorf("%w: resource revision binding does not match a published version", ErrNotFound)
	}
	return nil
}

// ValidatePinned verifies the immutable revision bytes and returns the bounded
// resource context injected into the scheduled turn.
func (s *Service) ValidatePinned(ctx context.Context, userID, taskID string, revision uint64, manifestHash string) (string, error) {
	v, err := s.store.GetResourceVersion(ctx, userID, taskID, revision)
	if err != nil {
		return "", fmt.Errorf("%w: version lookup: %v", ErrUnavailable, err)
	}
	if v == nil || v.ManifestHash != strings.TrimSpace(manifestHash) {
		return "", fmt.Errorf("%w: pinned resource revision is missing or does not match", ErrNotFound)
	}
	layout, err := NewLayout(s.workspace(userID), taskID)
	if err != nil {
		return "", err
	}
	m, err := VerifyPinned(layout.RevisionRoot(revision), v.ManifestHash)
	if err != nil {
		return "", err
	}
	return RenderContext(layout, revision, m, s.limits), nil
}

// CleanupStaging removes stale staging directories older than olderThan.
func (s *Service) CleanupStaging(ctx context.Context, userID, taskID string, olderThan time.Duration) error {
	if err := s.requireOwner(ctx, userID, taskID); err != nil {
		return err
	}
	layout, err := NewLayout(s.workspace(userID), taskID)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(layout.StagingRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(layout.StagingRoot(), e.Name()))
	}
	return nil
}

// DeleteUnreferencedVersion is the controlled maintenance path: it deletes an
// unused revision only after the Scheduler positively reports no task/occurrence
// reference.
func (s *Service) DeleteUnreferencedVersion(ctx context.Context, userID, taskID string, revision uint64, checker ReferenceChecker) (bool, error) {
	if checker == nil {
		return false, fmt.Errorf("%w: reference checker is not configured", ErrUnavailable)
	}
	referenced, err := checker.ResourceReferenced(ctx, taskID, revision)
	if err != nil {
		return false, fmt.Errorf("%w: reference check: %v", ErrUnavailable, err)
	}
	if referenced {
		return false, nil
	}
	v, err := s.store.GetResourceVersion(ctx, userID, taskID, revision)
	if err != nil {
		return false, fmt.Errorf("%w: version lookup: %v", ErrUnavailable, err)
	}
	if v == nil {
		return false, nil
	}
	layout, err := NewLayout(s.workspace(userID), taskID)
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(layout.RevisionRoot(revision)); err != nil {
		return false, err
	}
	return s.store.DeleteResourceVersion(ctx, userID, taskID, revision)
}

func (s *Service) requireOwner(ctx context.Context, userID, taskID string) error {
	ok, err := s.tasks.OwnsTask(ctx, userID, taskID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func versionInfo(v *model.ScheduledResourceVersion) *VersionInfo {
	info := &VersionInfo{
		TaskID: v.TaskID, Revision: v.Revision, ManifestHash: v.ManifestHash,
		FileCount: v.FileCount, TotalBytes: v.TotalBytes, CreateTime: v.CreateTime,
		Files: []FileEntry{},
	}
	var m Manifest
	if json.Unmarshal([]byte(v.ManifestJSON), &m) == nil && m.Files != nil {
		info.Files = m.Files
	}
	return info
}

func totalSize(files []FileEntry) int64 {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total
}

// RenderContext renders the bounded pinned-resource pointer. File contents are
// never inlined; the model reads them through the ordinary workspace tools.
func RenderContext(layout Layout, revision uint64, m *Manifest, limits Limits) string {
	if limits.MaxContextFiles <= 0 {
		limits.MaxContextFiles = 100
	}
	if limits.MaxContextBytes <= 0 {
		limits.MaxContextBytes = 4000
	}
	var b strings.Builder
	b.WriteString("<task-resources>\n")
	fmt.Fprintf(&b, "Pinned resource revision %d (manifest %s).\n", revision, m.ManifestHash)
	fmt.Fprintf(&b, "Files are available under the workspace at %s/revisions/%d/.\n",
		CronDirName+"/"+layout.TaskID, revision)
	b.WriteString("Read only what you need with xizhi_read_file; never execute resource files automatically.\n")
	omitted := 0
	for i, f := range m.Files {
		line := fmt.Sprintf("- %s (%d bytes)\n", f.Path, f.Size)
		if i >= limits.MaxContextFiles || b.Len()+len(line) > limits.MaxContextBytes {
			omitted = len(m.Files) - i
			break
		}
		b.WriteString(line)
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- ... %d more file(s); list the revision directory to see them.\n", omitted)
	}
	b.WriteString("</task-resources>")
	return b.String()
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
