package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// Layout is the server-resolved `.cron/<task_id>` tree inside one user's
// workspace. The client never supplies an absolute host path.
type Layout struct {
	WorkspaceRoot string
	TaskID        string
}

// NewLayout validates the task identity and builds the layout.
func NewLayout(workspaceRoot, taskID string) (Layout, error) {
	parsed, err := uuid.Parse(taskID)
	if err != nil || parsed.String() != taskID {
		return Layout{}, fmt.Errorf("%w: task id must be a canonical UUID", ErrInvalidRequest)
	}
	if workspaceRoot == "" {
		return Layout{}, fmt.Errorf("%w: workspace root is required", ErrInvalidRequest)
	}
	return Layout{WorkspaceRoot: workspaceRoot, TaskID: taskID}, nil
}

func (l Layout) CronRoot() string      { return filepath.Join(l.WorkspaceRoot, CronDirName) }
func (l Layout) TaskRoot() string      { return filepath.Join(l.CronRoot(), l.TaskID) }
func (l Layout) DraftRoot() string     { return filepath.Join(l.TaskRoot(), DraftDir) }
func (l Layout) RevisionsRoot() string { return filepath.Join(l.TaskRoot(), RevisionsDir) }
func (l Layout) StagingRoot() string   { return filepath.Join(l.TaskRoot(), StagingDir) }
func (l Layout) RevisionRoot(revision uint64) string {
	return filepath.Join(l.RevisionsRoot(), fmt.Sprintf("%d", revision))
}
func (l Layout) ManagedMarkerPath() string { return filepath.Join(l.CronRoot(), ManagedMarker) }
func (l Layout) TaskMarkerPath() string    { return filepath.Join(l.TaskRoot(), TaskMarker) }

type managedMarker struct {
	Version int    `json:"version"`
	UserID  string `json:"user_id"`
}

type taskMarker struct {
	Version int    `json:"version"`
	UserID  string `json:"user_id"`
	TaskID  string `json:"task_id"`
}

// EnsureLayout performs controlled initialization of the user-scoped `.cron`
// namespace. It never overwrites pre-existing unmanaged content: a non-empty
// `.cron` or task root without the ownership marker is a conflict.
func EnsureLayout(_ context.Context, workspaceRoot, userID, taskID string) (Layout, error) {
	layout, err := NewLayout(workspaceRoot, taskID)
	if err != nil {
		return Layout{}, err
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return Layout{}, err
	}
	if err := ensureManagedRoot(layout, userID); err != nil {
		return Layout{}, err
	}
	if err := ensureTaskRoot(layout, userID); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func ensureManagedRoot(layout Layout, userID string) error {
	root := layout.CronRoot()
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		if err := os.Mkdir(root, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		return writeMarkerOnce(layout.ManagedMarkerPath(), managedMarker{Version: 1, UserID: userID}, func(raw []byte) error {
			var m managedMarker
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("%w: managed marker is invalid", ErrNamespaceConflict)
			}
			if m.UserID != userID {
				return fmt.Errorf("%w: managed marker belongs to another user", ErrNamespaceConflict)
			}
			return nil
		})
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a real directory", ErrNamespaceConflict, CronDirName)
	}
	// Existing root: it is managed only if the marker exists.
	if _, err := os.Lstat(layout.ManagedMarkerPath()); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			return rerr
		}
		if len(entries) > 0 {
			return fmt.Errorf("%w: existing unmanaged %s content", ErrNamespaceConflict, CronDirName)
		}
		return writeMarkerOnce(layout.ManagedMarkerPath(), managedMarker{Version: 1, UserID: userID}, func(raw []byte) error {
			var m managedMarker
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("%w: managed marker is invalid", ErrNamespaceConflict)
			}
			if m.UserID != userID {
				return fmt.Errorf("%w: managed marker belongs to another user", ErrNamespaceConflict)
			}
			return nil
		})
	}
	raw, err := os.ReadFile(layout.ManagedMarkerPath())
	if err != nil {
		return err
	}
	var m managedMarker
	if err := json.Unmarshal(raw, &m); err != nil || m.Version != 1 {
		return fmt.Errorf("%w: managed marker is invalid", ErrNamespaceConflict)
	}
	if m.UserID != userID {
		return fmt.Errorf("%w: managed marker belongs to another user", ErrNamespaceConflict)
	}
	return nil
}

func ensureTaskRoot(layout Layout, userID string) error {
	root := layout.TaskRoot()
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		if err := os.Mkdir(root, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		if err := writeMarkerOnce(layout.TaskMarkerPath(), taskMarker{Version: 1, UserID: userID, TaskID: layout.TaskID}, taskMarkerValidator(userID, layout.TaskID)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: task root is not a real directory", ErrNamespaceConflict)
		}
		raw, merr := os.ReadFile(layout.TaskMarkerPath())
		if merr != nil {
			if !os.IsNotExist(merr) {
				return merr
			}
			entries, rerr := os.ReadDir(root)
			if rerr != nil {
				return rerr
			}
			if len(entries) > 0 {
				return fmt.Errorf("%w: existing unmanaged task root", ErrNamespaceConflict)
			}
			if err := writeMarkerOnce(layout.TaskMarkerPath(), taskMarker{Version: 1, UserID: userID, TaskID: layout.TaskID}, taskMarkerValidator(userID, layout.TaskID)); err != nil {
				return err
			}
		} else if err := taskMarkerValidator(userID, layout.TaskID)(raw); err != nil {
			return err
		}
	}
	for _, dir := range []string{layout.DraftRoot(), layout.RevisionsRoot(), layout.StagingRoot()} {
		di, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if di.Mode()&os.ModeSymlink != 0 || !di.IsDir() {
			return fmt.Errorf("%w: managed subdirectory is not a real directory", ErrNamespaceConflict)
		}
	}
	return nil
}

func taskMarkerValidator(userID, taskID string) func([]byte) error {
	return func(raw []byte) error {
		var m taskMarker
		if err := json.Unmarshal(raw, &m); err != nil || m.Version != 1 {
			return fmt.Errorf("%w: task marker is invalid", ErrNamespaceConflict)
		}
		if m.UserID != userID || m.TaskID != taskID {
			return fmt.Errorf("%w: task marker belongs to another task/user", ErrNamespaceConflict)
		}
		return nil
	}
}

func writeMarkerOnce(path string, value any, validate func([]byte) error) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if validate != nil {
			return validate(existing)
		}
		return nil
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
