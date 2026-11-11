// Package scheduledcontract holds pure, ownership-free values shared by the
// Scheduler and the Runtime. It has no store, LLM, workspace or agent
// dependency.
package scheduledcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// FingerprintInput is the non-secret configuration identity of one scheduled
// occurrence. Attempt and claim/fence values are deliberately excluded: they
// are retry control, so a re-submission after a fenced no-start attempt keeps
// the same fingerprint and resolves to the same receipt.
type FingerprintInput struct {
	TaskID           string
	OccurrenceID     string
	RunID            string
	SessionID        string
	SnapshotJSON     string
	ResourceRevision uint64
	ManifestHash     string
}

// Fingerprint returns the stable SHA-256 identity of an occurrence's
// configuration. Both sides compute it from the Scheduler's authoritative
// snapshot string, so the comparison is byte-exact.
func Fingerprint(in FingerprintInput) string {
	parts := []string{
		in.TaskID,
		in.OccurrenceID,
		in.RunID,
		in.SessionID,
		in.SnapshotJSON,
		strings.TrimSpace(in.ManifestHash),
		uintString(in.ResourceRevision),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func uintString(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
