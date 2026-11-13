package storage

// fstype check outcomes shared by the platform-specific mount_*.go files and
// the portable CheckSharedBackend in storage.go.
const (
	// fstypeUnsupported means the platform cannot inspect the filesystem type
	// (non-Linux). The caller skips the fstype guard with a warning, relying on
	// the portable writable probe alone.
	fstypeUnsupported = -1
	// fstypeOther means the path is on a filesystem that is NOT FUSE-family,
	// i.e. it does not look like the expected shared POSIX filesystem.
	fstypeOther = 0
	// fstypeFUSE means the path sits on a FUSE-family filesystem, which is what
	// a MinIO-backed JuiceFS mount presents as.
	fstypeFUSE = 1
)
