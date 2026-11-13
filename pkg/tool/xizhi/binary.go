package xizhi

import (
	"bytes"
	"io"
	"os"
)

// binarySniffPeek is the number of leading bytes examined for a NUL byte when
// deciding whether a file is binary. It mirrors ripgrep's binary-detection
// window and the previous grep heuristic (8 KiB).
const binarySniffPeek = 8192

// looksBinary reports whether data appears to be binary by checking for a NUL
// byte in its leading bytes. It examines at most binarySniffPeek bytes so even a
// huge file is classified cheaply. This consolidates the workspace tools behind
// one rule: read uses it to reject binary files, and the Go grep engine uses it
// to skip them. (The rg engine relies on ripgrep skipping binary files during
// directory traversal, but pre-sniffs an explicitly targeted single file — see
// binarySniffFile.)
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffPeek)], 0) >= 0
}

// binarySniffFile reports whether the file at absPath looks binary (a NUL byte
// in its leading bytes). An unreadable file reports false, leaving the decision
// to the caller's normal path. The rg grep engine uses it in single-file mode:
// ripgrep skips binary files found by directory traversal natively, but searches
// an explicitly given file with quit-on-NUL semantics (matches before the first
// NUL are reported) — the pre-sniff aligns that case with the skip behavior of
// the Go engine and of directory mode.
func binarySniffFile(absPath string) bool {
	f, err := os.Open(absPath)
	if err != nil {
		return false
	}
	defer f.Close()
	preview := make([]byte, binarySniffPeek)
	n, _ := io.ReadFull(f, preview)
	return looksBinary(preview[:n])
}
