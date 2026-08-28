package reqctx

import (
	"context"
	"testing"
)

func TestWithSessionID_RoundTrip(t *testing.T) {
	ctx := WithSessionID(context.Background(), "sess-123")
	if got := SessionIDFromContext(ctx); got != "sess-123" {
		t.Fatalf("SessionIDFromContext = %q, want %q", got, "sess-123")
	}
}

func TestWithSessionID_EmptyIDReturnsOriginalContext(t *testing.T) {
	// An empty id must not write a value: the original ctx comes back and a
	// later read reports none (matching trace.WithContext semantics).
	base := context.Background()
	ctx := WithSessionID(base, "")
	if got := SessionIDFromContext(ctx); got != "" {
		t.Fatalf("SessionIDFromContext after empty inject = %q, want empty", got)
	}
	if ctx != base {
		t.Fatal("WithSessionID(empty) must return the original ctx unchanged")
	}
}

func TestSessionIDFromContext_NoValue(t *testing.T) {
	if got := SessionIDFromContext(context.Background()); got != "" {
		t.Fatalf("SessionIDFromContext on undecorated ctx = %q, want empty", got)
	}
}

func TestSessionIDFromContext_NilContextSafe(t *testing.T) {
	// A nil ctx (defensive callers) must not panic. Routed through a typed
	// variable so staticcheck's SA1012 (no nil Context) sees an intentional
	// interface value rather than a literal.
	var nilCtx context.Context
	if got := SessionIDFromContext(nilCtx); got != "" {
		t.Fatalf("SessionIDFromContext(nil) = %q, want empty", got)
	}
}

func TestWithSessionID_LastWriteWins(t *testing.T) {
	// Re-injecting over an existing value replaces it — the turn path relies
	// on one authoritative id per ctx tree.
	ctx := WithSessionID(context.Background(), "first")
	ctx = WithSessionID(ctx, "second")
	if got := SessionIDFromContext(ctx); got != "second" {
		t.Fatalf("SessionIDFromContext after re-inject = %q, want %q", got, "second")
	}
}
