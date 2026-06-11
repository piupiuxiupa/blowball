package trace

import (
	"testing"
)

func TestNew_ReturnsNonEmptyUniqueUUID(t *testing.T) {
	id := New()
	if id == "" {
		t.Fatal("New() returned empty string")
	}
	// UUID v4 string is 36 chars including hyphens.
	if len(id) != 36 {
		t.Errorf("New() length = %d, want 36 (UUID v4)", len(id))
	}
	// Version nibble at position 14 must be '4'.
	if id[14] != '4' {
		t.Errorf("New() = %q, expected UUID v4 (version nibble '4' at position 14)", id)
	}

	// Uniqueness across many calls.
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("New() produced duplicate id %q after %d calls", id, i)
		}
		seen[id] = struct{}{}
	}
}
