package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/reqctx"
	"github.com/lush/blowball/internal/pkg/trace"
)

// withObservedDefault swaps the package default logger for an observer-backed
// one so the dispatch log's fields can be asserted. Returns the logs core.
func withObservedDefault(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	prev := logger.L()
	logger.SetDefault(zap.New(core))
	t.Cleanup(func() { logger.SetDefault(prev) })
	return logs
}

// fieldMap flattens an entry's context fields into key → value. String and
// duration fields carry their typed values; anything else falls back to the
// field's Interface (zap stores duration in Integer, not Interface).
func fieldMap(entry observer.LoggedEntry) map[string]any {
	m := make(map[string]any, len(entry.Context))
	for _, f := range entry.Context {
		switch f.Type {
		case zapcore.StringType:
			m[f.Key] = f.String
		case zapcore.DurationType:
			m[f.Key] = time.Duration(f.Integer)
		default:
			m[f.Key] = f.Interface
		}
	}
	return m
}

// correlatedCtx builds a ctx carrying both correlation ids, the shape Registry
// sees on the turn path (session injected at the handler entry, trace at the
// middleware).
func correlatedCtx() context.Context {
	return reqctx.WithSessionID(trace.WithContext(context.Background(), "trace-log"), "session-log")
}

// TestCall_DispatchLog_SuccessInfo asserts the central dispatch log on the
// happy path: one INFO entry with event/tool/duration/args_preview plus the
// ctx correlation fields, and no result body.
func TestCall_DispatchLog_SuccessInfo(t *testing.T) {
	logs := withObservedDefault(t)

	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("xizhi_read_file")))
	res, err := r.Call(correlatedCtx(), "xizhi_read_file", json.RawMessage(`{"path":"a.txt"}`))
	require.NoError(t, err)
	require.NotNil(t, res)

	entries := logs.FilterMessage("registry tool call completed").TakeAll()
	require.Len(t, entries, 1, "expected exactly one dispatch INFO entry")
	m := fieldMap(entries[0])
	assert.Equal(t, "tool_call", m["event"])
	assert.Equal(t, "xizhi_read_file", m["tool"])
	assert.Equal(t, "session-log", m["session_id"])
	assert.Equal(t, "trace-log", m["trace_id"])
	assert.Equal(t, `{"path":"a.txt"}`, m["args_preview"])
	assert.NotNil(t, m["duration"], "duration field must be present")
	assert.NotContains(t, m, "error", "success entry must carry no error field")
	// The result body must never be logged.
	for _, f := range entries[0].Context {
		assert.NotEqual(t, "result", f.Key)
	}
}

// TestCall_DispatchLog_FailureWarn asserts the failure path: a WARN entry
// carrying the error field in addition to the success field set.
func TestCall_DispatchLog_FailureWarn(t *testing.T) {
	logs := withObservedDefault(t)

	r := NewRegistry()
	spec := newSpec("boom")
	spec.Execute = func(ctx context.Context, args json.RawMessage) (any, error) {
		return nil, errors.New("kaboom")
	}
	require.NoError(t, r.Register(spec))

	_, err := r.Call(correlatedCtx(), "boom", json.RawMessage(`{}`))
	require.Error(t, err)

	entries := logs.FilterMessage("registry tool call failed").TakeAll()
	require.Len(t, entries, 1, "expected exactly one dispatch WARN entry")
	assert.Equal(t, zapcore.WarnLevel, entries[0].Level)
	m := fieldMap(entries[0])
	assert.Equal(t, "tool_call", m["event"])
	assert.Equal(t, "boom", m["tool"])
	assert.Equal(t, "session-log", m["session_id"])
	assert.Equal(t, "trace-log", m["trace_id"])
	assert.NotNil(t, m["duration"])
	errField, ok := entries[0].ContextMap()["error"]
	require.True(t, ok, "failure entry must carry the error field")
	assert.Contains(t, errField.(string), "kaboom")

	// No success INFO was emitted for the failed call.
	assert.Len(t, logs.FilterMessage("registry tool call completed").TakeAll(), 0)
}

// TestCall_DispatchLog_TimeoutPathAlsoLogs asserts a timeout-truncated call
// still produces its dispatch WARN (the log must survive every terminal path
// of Execute, including ctx cancellation).
func TestCall_DispatchLog_TimeoutPathAlsoLogs(t *testing.T) {
	logs := withObservedDefault(t)

	r := NewRegistry()
	r.SetTimeouts(map[string]time.Duration{"slow": 20 * time.Millisecond})
	require.NoError(t, r.Register(blockingSpec("slow", make(chan struct{}))))

	_, err := r.Call(correlatedCtx(), "slow", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))

	entries := logs.FilterMessage("registry tool call failed").TakeAll()
	require.Len(t, entries, 1, "timeout path must produce the dispatch WARN")
	m := fieldMap(entries[0])
	assert.Equal(t, "tool_call", m["event"])
	assert.Equal(t, "slow", m["tool"])
	assert.NotNil(t, m["duration"])
}

// TestCall_DispatchLog_NoCtxIDsOmittedFields asserts graceful omission: with a
// bare ctx the dispatch log carries no session_id/trace_id keys at all.
func TestCall_DispatchLog_NoCtxIDsOmittedFields(t *testing.T) {
	logs := withObservedDefault(t)

	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("bare")))
	_, err := r.Call(context.Background(), "bare", json.RawMessage(`{}`))
	require.NoError(t, err)

	entries := logs.FilterMessage("registry tool call completed").TakeAll()
	require.Len(t, entries, 1)
	m := fieldMap(entries[0])
	assert.NotContains(t, m, "session_id")
	assert.NotContains(t, m, "trace_id")
}

// TestTruncateArgsPreview pins the preview budget: short args pass through
// verbatim, long args cut at argsPreviewLimit runes with the ellipsis marker,
// empty renders empty.
func TestTruncateArgsPreview(t *testing.T) {
	assert.Equal(t, "", truncateArgsPreview(nil))
	assert.Equal(t, `{"a":1}`, truncateArgsPreview(json.RawMessage(`{"a":1}`)))

	long := json.RawMessage(`{"q":"` + strings.Repeat("字", argsPreviewLimit+50) + `"}`)
	got := truncateArgsPreview(long)
	runes := []rune(got)
	assert.Len(t, runes, argsPreviewLimit+1, "truncated preview must be limit runes + ellipsis")
	assert.True(t, strings.HasSuffix(got, "…"), "truncated preview must end with the ellipsis marker")
}
