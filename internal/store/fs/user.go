package fs

import (
	"context"
	"fmt"
	"path/filepath"
)

// userSubDirs lists the fixed subdirectories created directly under each user's
// directory. The order is the on-disk creation order; sessions/ first because
// the session service depends on it immediately, then workspace/. Per-user
// skills are NOT a top-level sibling: they live under the workspace in the
// reserved .blowball/skills/ namespace, created by EnsureUserDirs once the
// workspace dir exists.
var userSubDirs = []string{"sessions", "workspace"}

// userDir returns the directory owned by userID under the configured root.
func (s *Store) userDir(userID string) string {
	return filepath.Join(s.root, userID)
}

// EnsureUserDirs creates the user directory for userID, the canonical sessions/
// and workspace/ subdirectories beneath it, and the reserved
// workspace/.blowball/skills/ directory that holds per-user skills. The call is
// idempotent: already-existing directories are kept untouched.
//
// The function is named to align with the workspace-api spec's
// "Auto create user directories" requirement, which is triggered on first
// login / first per-user operation.
func (s *Store) EnsureUserDirs(ctx context.Context, userID string) error {
	if userID == "" {
		return fmt.Errorf("fs store: userID is empty")
	}
	base := s.userDir(userID)
	logFS(ctx, "user.ensure_dirs", base)

	// MkdirAll on the base covers the root itself when it doesn't yet exist.
	if err := mkdirAll(base); err != nil {
		return fmt.Errorf("create user dir %q: %w", base, err)
	}

	for _, sub := range userSubDirs {
		dir := filepath.Join(base, sub)
		if err := mkdirAll(dir); err != nil {
			return fmt.Errorf("create user subdir %q: %w", dir, err)
		}
	}

	// Per-user skills live under the workspace in the reserved .blowball
	// namespace; create it after the workspace dir exists. (No top-level
	// skills/ sibling is created.)
	skillsDir := s.UserSkills(userID)
	if err := mkdirAll(skillsDir); err != nil {
		return fmt.Errorf("create user skills dir %q: %w", skillsDir, err)
	}
	return nil
}

// UserWorkspace returns the absolute path of userID's workspace directory. It
// is a convenience accessor for the Xizhi tools (read/write/modify) that
// operate exclusively under this prefix.
func (s *Store) UserWorkspace(userID string) string {
	return filepath.Join(s.userDir(userID), "workspace")
}

// UserSkills returns the absolute path of userID's per-user skills directory.
// Skills live under the workspace in the reserved, hidden .blowball/skills/
// namespace (data/{userID}/workspace/.blowball/skills) so they travel with the
// workspace; the skill list handler and luban install tool both scan/write it.
// The .blowball name is reserved only on the agent path: xizhi.ValidatePath
// rejects it so the agent's file tools cannot tamper with skills, while the
// REST workspace API (xizhi.ValidatePathAllowReserved) and the luban_* tools
// can still read and write it, letting the user manage their own skills.
func (s *Store) UserSkills(userID string) string {
	return filepath.Join(s.userDir(userID), "workspace", ".blowball", "skills")
}

// UserSessions returns the absolute path of userID's sessions directory. The
// FS session store writes session JSON files here.
func (s *Store) UserSessions(userID string) string {
	return filepath.Join(s.userDir(userID), "sessions")
}

// UserDirExists reports whether the base user directory (and the expected
// subdirectories) exist for userID. It is a cheap way for the service layer to
// decide whether to call EnsureUserDirs.
func (s *Store) UserDirExists(userID string) bool {
	for _, sub := range userSubDirs {
		if !dirExists(filepath.Join(s.userDir(userID), sub)) {
			return false
		}
	}
	return true
}
