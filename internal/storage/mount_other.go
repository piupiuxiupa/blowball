//go:build !linux

package storage

// sharedFSType is the non-Linux stub. statfs(2) fstype semantics differ across
// platforms (and FUSE/Landlock/bwrap are Linux-only), so the fstype guard is a
// Linux-only feature. On dev platforms (macOS/Windows) the caller logs a
// warning and relies on the portable writable probe; deployments normally use
// backend: local there anyway.
func sharedFSType(path string) (int, uint64, error) {
	return fstypeUnsupported, 0, nil
}
