package logger

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/internal/pkg/reqctx"
	"github.com/lush/blowball/internal/pkg/trace"
)

// withObservedDefault installs an observer-backed logger as the package
// default for the test's duration and returns the recorded-log core.
func withObservedDefault(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	prev := L()
	SetDefault(zap.New(core))
	t.Cleanup(func() { SetDefault(prev) })
	return logs
}

// fieldKeys returns the context field keys of an entry, in order.
func fieldKeys(entry observer.LoggedEntry) []string {
	keys := make([]string, 0, len(entry.Context))
	for _, f := range entry.Context {
		keys = append(keys, f.Key)
	}
	return keys
}

// TestFromContext_AttachesBothIDsWhenPresent covers the turn path: the
// streaming handler injects session_id and the middleware trace_id, and every
// log through FromContext carries both.
func TestFromContext_AttachesBothIDsWhenPresent(t *testing.T) {
	logs := withObservedDefault(t)

	ctx := reqctx.WithSessionID(trace.WithContext(context.Background(), "tid-1"), "sid-1")
	FromContext(ctx).Info("both ids")

	entries := logs.TakeAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	fields := entries[0].Context
	want := map[string]string{"session_id": "sid-1", "trace_id": "tid-1"}
	got := map[string]string{}
	for _, f := range fields {
		if f.Type == zapcore.StringType {
			got[f.Key] = f.String
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %s = %q, want %q (all fields: %v)", k, got[k], v, fieldKeys(entries[0]))
		}
	}
}

// TestFromContext_OnlyTraceID covers the api-role / background paths: ctx
// carries trace_id but no session_id — the session_id field is omitted, never
// emitted as an empty string.
func TestFromContext_OnlyTraceID(t *testing.T) {
	logs := withObservedDefault(t)

	FromContext(trace.WithContext(context.Background(), "tid-2")).Info("trace only")

	entries := logs.TakeAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	for _, f := range entries[0].Context {
		if f.Key == "session_id" {
			t.Errorf("session_id must be omitted when absent, got %q", f.String)
		}
	}
	found := false
	for _, f := range entries[0].Context {
		if f.Key == "trace_id" && f.String == "tid-2" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace_id field missing or wrong value: %v", fieldKeys(entries[0]))
	}
}

// TestFromContext_NoIDsMatchesPlainL covers a bare ctx: no fields are added,
// behavior is identical to L().
func TestFromContext_NoIDsMatchesPlainL(t *testing.T) {
	logs := withObservedDefault(t)

	FromContext(context.Background()).Info("no ids")

	entries := logs.TakeAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].Context) != 0 {
		t.Errorf("expected zero context fields, got %v", fieldKeys(entries[0]))
	}
}

// TestFromContext_NilContextReturnsPlainDefault pins the nil-safety contract:
// FromContext(nil) == L(). The nil goes through a typed variable so
// staticcheck's SA1012 (no nil Context) sees an intentional interface value.
func TestFromContext_NilContextReturnsPlainDefault(t *testing.T) {
	logs := withObservedDefault(t)

	var nilCtx context.Context
	FromContext(nilCtx).Info("nil ctx")

	entries := logs.TakeAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].Context) != 0 {
		t.Errorf("expected zero context fields on nil ctx, got %v", fieldKeys(entries[0]))
	}
}

// TestFromContext_EmptyStringIDsAreOmitted guards the graceful-omission rule
// against explicitly-injected empty values (WithSessionID("") / trace "").
func TestFromContext_EmptyStringIDsAreOmitted(t *testing.T) {
	logs := withObservedDefault(t)

	FromContext(trace.WithContext(reqctx.WithSessionID(context.Background(), "sid-3"), "")).Info("empty trace")

	entries := logs.TakeAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	for _, f := range entries[0].Context {
		if f.Key == "trace_id" {
			t.Errorf("empty trace_id must be omitted, not emitted")
		}
	}
}

// TestFromContext_WrapsEveryCallSiteField checks the With-chaining contract:
// fields added at the call site land after the correlation fields, and the
// correlation prefix survives reuse of the returned logger.
func TestFromContext_WrapsEveryCallSiteField(t *testing.T) {
	logs := withObservedDefault(t)

	ctx := reqctx.WithSessionID(trace.WithContext(context.Background(), "tid-4"), "sid-4")
	l := FromContext(ctx).With(zap.String("op", "unit-test"))
	l.Info("chained")
	l.Warn("reused")

	entries := logs.TakeAll()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	for i, e := range entries {
		keys := fieldKeys(e)
		// session_id and trace_id come first (With prefix), the call-site
		// field last.
		if keys[0] != "session_id" || keys[1] != "trace_id" || keys[len(keys)-1] != "op" {
			t.Errorf("entry %d field order = %v, want [session_id trace_id ... op]", i, keys)
		}
	}
}
