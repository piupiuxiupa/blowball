//go:build linux

package storage

import (
	"golang.org/x/sys/unix"
)

// sharedFSType reports whether path sits on a FUSE-family filesystem, which is
// how a MinIO-backed JuiceFS mount presents itself to the kernel (its statfs
// magic is FUSE_SUPER_MAGIC regardless of the underlying object store). The
// second return value is the raw filesystem magic number, surfaced only for the
// "not a shared FS" error message so operators can see what was found instead.
//
// On Linux this uses statfs(2). JuiceFS (and any FUSE filesystem) reports
// FUSE_SUPER_MAGIC; a plain local filesystem (ext4/xfs/tmpfs/...) reports a
// different magic, which is exactly the "operator forgot to mount" signal the
// health check wants to catch. Matching the FUSE family (rather than a JuiceFS-
// specific string) sidesteps the kernel-version variance in fstype names noted
// in the change's open questions — the kernel magic is stable.
func sharedFSType(path string) (int, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fstypeOther, 0, err
	}
	magic := uint64(stat.Type)
	if magic == uint64(unix.FUSE_SUPER_MAGIC) {
		return fstypeFUSE, magic, nil
	}
	return fstypeOther, magic, nil
}
