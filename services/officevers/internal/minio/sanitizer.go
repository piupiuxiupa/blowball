package minio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
)

const (
	maxSegmentLen = 255
	// maxPathLen bounds the total length of a client-supplied logical path. The
	// path is not stored verbatim (it is hashed), so this is a hygiene guard, not
	// an S3 key-length constraint.
	maxPathLen = 1024
)

// Validation errors. They are wrapped values so callers can distinguish
// client-side validation failures (HTTP 400) from other failures.
var (
	ErrEmptyUUID      = errors.New("uuid must not be empty")
	ErrEmptyPath      = errors.New("logical path must not be empty")
	ErrInvalidUUID    = errors.New("uuid contains invalid characters")
	ErrInvalidPath    = errors.New("logical path contains invalid segments or characters")
	ErrSegmentTooLong = errors.New("name exceeds maximum length")
	ErrEmptyVersionID = errors.New("version id must not be empty")
)

// IsValidationError reports whether err is a client-side validation error.
func IsValidationError(err error) bool {
	switch {
	case errors.Is(err, ErrEmptyUUID),
		errors.Is(err, ErrEmptyPath),
		errors.Is(err, ErrInvalidUUID),
		errors.Is(err, ErrInvalidPath),
		errors.Is(err, ErrSegmentTooLong),
		errors.Is(err, ErrEmptyVersionID):
		return true
	}
	return false
}

// validateUUIDSegment validates the caller-supplied directory UUID as a single
// path segment. It rejects empty values, path separators ("/", "\""),
// parent/current-directory segments (".", ".."), and C0 control characters.
func validateUUIDSegment(value string) error {
	if value == "" {
		return ErrEmptyUUID
	}
	if len(value) > maxSegmentLen {
		return ErrSegmentTooLong
	}
	if value == "." || value == ".." {
		return ErrInvalidUUID
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '/' || ch == '\\' {
			return ErrInvalidUUID
		}
		// Reject NUL and other C0 control characters.
		if ch < 0x20 {
			return ErrInvalidUUID
		}
	}
	return nil
}

// ValidateUUID validates a caller-supplied directory UUID. It enforces that the
// UUID is a single path segment (no separators, no traversal, no controls) but
// does not require a specific UUID format, since callers may use other
// identifiers.
func ValidateUUID(uuid string) error {
	return validateUUIDSegment(uuid)
}

// ValidateLogicalPath validates a client-supplied logical path. The path MAY
// contain "/"-separated segments (a directory hierarchy). It rejects:
//   - empty paths;
//   - a leading or trailing "/";
//   - empty segments ("a//b");
//   - "." or ".." segments (path traversal is structurally impossible once the
//     path is hashed, but these are rejected to enforce a single canonical form
//     and to catch client bugs);
//   - any segment longer than maxSegmentLen, or a total path over maxPathLen;
//   - NUL or other C0 control characters, and backslashes (the logical path
//     uses "/" as its only separator).
//
// No Unicode normalization or case-folding is applied: the exact bytes
// supplied by the client are what get hashed, so two byte-different variants of
// a path map to different storage keys.
func ValidateLogicalPath(p string) error {
	if p == "" {
		return ErrEmptyPath
	}
	if len(p) > maxPathLen {
		return ErrSegmentTooLong
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return ErrInvalidPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidPath
		}
		if len(seg) > maxSegmentLen {
			return ErrSegmentTooLong
		}
		for i := 0; i < len(seg); i++ {
			ch := seg[i]
			if ch == '\\' || ch < 0x20 {
				return ErrInvalidPath
			}
		}
	}
	return nil
}

// StorageLeaf derives the storage object leaf name for a logical path. It
// validates the path, then returns the lowercase hex SHA-256 digest of the
// path's UTF-8 bytes — the ENTIRE path, extension included — with the path's
// original extension appended verbatim. The hash is deterministic on the path
// name only (never on file content), so the same logical path always resolves
// to the same storage key and continues to accumulate versions.
func StorageLeaf(p string) (string, error) {
	if err := ValidateLogicalPath(p); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:]) + path.Ext(p), nil
}

// ObjectKey builds the MinIO object key for a file in a UUID directory. The
// leaf is expected to be the already-hashed storage leaf produced by
// StorageLeaf, not the client's raw logical path.
func ObjectKey(uuid, leaf string) string {
	return uuid + "/" + leaf
}
