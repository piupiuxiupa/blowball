package resource

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
)

// Write operation kinds. They are plain strings so the xizhi tool package can
// declare the same minimal PathGuard interface without importing this package.
const (
	WriteCreate       = "create"
	WriteModify       = "modify"
	WriteDelete       = "delete"
	WriteRenameSource = "rename_source"
	WriteRenameTarget = "rename_target"
)

// Guard enforces the `.cron` managed-namespace rules across every generic
// workspace write entry point (REST text/upload/create/delete/rename, Office
// write-back, and the xizhi file tools).
//
// Outside `.cron` the guard is a no-op (existing behavior). Inside:
//   - only paths strictly below `.cron/<task_id>/draft/` may be written;
//   - the `.cron` root, the task root, `revisions/`, `.staging/`, markers and
//     every other segment are read-only;
//   - task ownership is verified through the Scheduler (fail closed).
type Guard struct {
	svc *Service
}

// NewGuard wraps the resource service.
func NewGuard(svc *Service) *Guard { return &Guard{svc: svc} }

// CheckWrite validates one mutation. It returns nil when the path is outside
// the managed namespace or is an authorized draft path (after bumping the
// draft fence).
func (g *Guard) CheckWrite(ctx context.Context, userID, relPath, kind string) error {
	if g == nil || g.svc == nil {
		return nil
	}
	clean := cleanRel(relPath)
	if clean == "" {
		return nil
	}
	segments := strings.Split(clean, "/")
	if segments[0] != CronDirName {
		return nil
	}
	if len(segments) < 3 {
		// `.cron` and `.cron/<task>` are managed roots.
		return fmt.Errorf("%w: managed namespace root is read-only", ErrReadOnly)
	}
	taskID := segments[1]
	if _, err := uuid.Parse(taskID); err != nil {
		return fmt.Errorf("%w: unrecognized managed task path", ErrReadOnly)
	}
	if segments[2] != DraftDir {
		return fmt.Errorf("%w: only the authorized draft is writable", ErrReadOnly)
	}
	if len(segments) == 3 {
		// The draft root itself is created by controlled initialization.
		return fmt.Errorf("%w: draft root is read-only", ErrReadOnly)
	}
	// Rename sources and targets share the same draft-only rule; both calls
	// must pass, so an ancestor/move bypass cannot slip through.
	switch kind {
	case WriteCreate, WriteModify, WriteDelete, WriteRenameSource, WriteRenameTarget:
	default:
		return fmt.Errorf("%w: unknown write operation", ErrReadOnly)
	}
	if err := g.svc.BumpDraft(ctx, userID, taskID); err != nil {
		return err
	}
	return nil
}

// CheckRead is intentionally permissive inside the owner's workspace: the
// per-user path scoping already restricts reads to the caller, and reading a
// task's draft/published revision is explicitly allowed. It only rejects
// escaped paths.
func (g *Guard) CheckRead(relPath string) error {
	clean := cleanRel(relPath)
	if clean == "" {
		return nil
	}
	if strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return ErrInvalidPath
	}
	return nil
}

func cleanRel(relPath string) string {
	relPath = strings.ReplaceAll(relPath, "\\", "/")
	clean := path.Clean("/" + relPath)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." {
		return ""
	}
	return clean
}
