package xizhi

import "bytes"

// binarySniffPeek is the number of leading bytes examined for a NUL byte when
// deciding whether a file is binary. It mirrors ripgrep's binary-detection
// window and the previous grep heuristic (8 KiB).
const binarySniffPeek = 8192

// looksBinary reports whether data appears to be binary by checking for a NUL
// byte in its leading bytes. It examines at most binarySniffPeek bytes so even a
// huge file is classified cheaply. This consolidates the workspace tools behind
// one rule: read uses it to reject binary files, and the Go grep engine uses it
// to skip them. (ripgrep skips binary files natively, so the rg engine does not
// call this.)
func looksBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniffPeek)], 0) >= 0
}
