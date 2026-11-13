package scheduledcontract

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func base() FingerprintInput {
	return FingerprintInput{
		TaskID: "task-1", OccurrenceID: "occ-1", RunID: "run-1", SessionID: "sess-1",
		SnapshotJSON: `{"prompt":"do it"}`, ResourceRevision: 0,
	}
}

func TestFingerprintIsStable(t *testing.T) {
	assert.Equal(t, Fingerprint(base()), Fingerprint(base()))
	assert.Len(t, Fingerprint(base()), 64)
}

func TestFingerprintCoversConfigurationNotRetryControl(t *testing.T) {
	in := base()
	changed := in
	changed.SnapshotJSON = `{"prompt":"do something else"}`
	assert.NotEqual(t, Fingerprint(in), Fingerprint(changed))

	changed = in
	changed.ResourceRevision = 3
	assert.NotEqual(t, Fingerprint(in), Fingerprint(changed))

	changed = in
	changed.ManifestHash = strings.Repeat("a", 64)
	assert.NotEqual(t, Fingerprint(in), Fingerprint(changed))

	changed = in
	changed.RunID = "run-2"
	assert.NotEqual(t, Fingerprint(in), Fingerprint(changed))
}
