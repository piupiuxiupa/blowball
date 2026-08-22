package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/tokens"
	"github.com/lush/blowball/internal/tool/mcpclient"
)

// newSpillManager stands up a Manager over a temp workspace with a fake
// transport answering tools/call with callRes, and the given inline cap.
func newSpillManager(t *testing.T, cap int, callRes *mcpclient.ToolsCallResult) (*Manager, string, *fakeTransport) {
	t.Helper()
	ft := &fakeTransport{
		tools:      []mcpclient.Tool{{Name: "query", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		callResult: callRes,
	}
	m, ws := newManagerWithFake(t, ft, ManagerOptions{ConnectTimeout: time.Second, CallTimeout: time.Second, MaxInlineResultTokens: cap})
	writeServers(t, ws, Server{
		Name: "gildata", URL: "http://x", Transport: "http",
		Tools: []ToolCache{{Name: "query", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	return m, ws, ft
}

// bigText builds a single text content item whose estimate comfortably
// exceeds the given token cap (ASCII → ceil(chars/4)).
func bigText(cap int) mcpclient.Content {
	return mcpclient.Content{Type: "text", Text: strings.Repeat("x", cap*8)}
}

func TestCallTool_OversizedResultSpilled(t *testing.T) {
	// 200*8 = 1600 ASCII chars: comfortably over both the 100-token cap and
	// the 1KB preview, so the envelope's truncation marker is exercised.
	m, ws, _ := newSpillManager(t, 100, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{bigText(200)},
	})
	defer m.Close()

	out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "")
	require.NoError(t, err)
	env, ok := out.(spillEnvelope)
	require.True(t, ok, "oversized result must return a spillEnvelope, got %T", out)
	assert.True(t, env.Spilled)
	assert.Contains(t, env.Path, "tmp/mcp-outputs/gildata/query-")
	assert.Greater(t, env.TokensEst, 100)
	assert.Equal(t, 200*8, env.Size, "size is the raw-text byte length")
	assert.Equal(t, 1, env.Items)
	assert.Len(t, env.Preview, previewBytes+len("…[truncated]"))
	assert.Contains(t, env.Hint, "xizhi_read_file")

	// The spill file holds the single text item verbatim (raw text, .txt).
	abs := filepath.Join(ws, filepath.FromSlash(env.Path))
	data, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("x", 200*8), string(data))
	assert.True(t, strings.HasSuffix(env.Path, ".txt"))
}

func TestSpill_BoundaryIsInclusive(t *testing.T) {
	// Pin the boundary directly: with the cap set to the result's own
	// estimate (which includes the JSON envelope overhead by design), the
	// result stays inline; one token over, it spills.
	m, _, _ := newSpillManager(t, 1, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{{Type: "text", Text: strings.Repeat("x", 400)}},
	})
	defer m.Close()
	res := callResult{Content: []mcpclient.Content{{Type: "text", Text: strings.Repeat("x", 400)}}}
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	est := tokens.Estimate(string(raw))

	m.maxInlineResultTokens = est
	out := m.maybeSpill("s", "t", "", "trace", res)
	_, ok := out.(callResult)
	assert.True(t, ok, "est == cap must stay inline, got %T", out)

	m.maxInlineResultTokens = est - 1
	out = m.maybeSpill("s", "t", "", "trace", res)
	_, ok = out.(spillEnvelope)
	assert.True(t, ok, "est == cap+1 must spill, got %T", out)
}

func TestCallTool_DisabledCapNeverSpills(t *testing.T) {
	m, _, _ := newSpillManager(t, 0, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{bigText(100000)},
	})
	defer m.Close()

	out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "")
	require.NoError(t, err)
	_, ok := out.(callResult)
	require.True(t, ok, "cap 0 disables the automatic spill entirely, got %T", out)
}

func TestCallTool_ForcedOutputPathSpillsRegardlessOfSize(t *testing.T) {
	m, ws, _ := newSpillManager(t, 0, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{{Type: "text", Text: "tiny"}},
	})
	defer m.Close()

	out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "data/gil.txt")
	require.NoError(t, err)
	env, ok := out.(spillEnvelope)
	require.True(t, ok, "output_path forces a spill even under the (disabled) cap, got %T", out)
	assert.Equal(t, "data/gil.txt", env.Path)

	// Parent directories are auto-created; content is the raw single text.
	data, err := os.ReadFile(filepath.Join(ws, "data", "gil.txt"))
	require.NoError(t, err)
	assert.Equal(t, "tiny", string(data))

	// A second forced call to the same path overwrites (create-or-overwrite).
	_, err = callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "data/gil.txt")
	require.NoError(t, err)
	data, err = os.ReadFile(filepath.Join(ws, "data", "gil.txt"))
	require.NoError(t, err)
	assert.Equal(t, "tiny", string(data), "second write must not fail with exists")
}

func TestCallTool_InvalidOutputPathRejectedBeforeCall(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"absolute", "/etc/passwd", "workspace-relative"},
		{"traversal", "a/../../escape.txt", "output_path invalid"},
		{"reserved namespace", ".blowball/leak.json", "output_path invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, ft := newSpillManager(t, 0, &mcpclient.ToolsCallResult{
				Content: []mcpclient.Content{{Type: "text", Text: "ok"}},
			})
			defer m.Close()

			_, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), tc.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			_, _, calls := ft.snapshot()
			assert.Equal(t, 0, calls, "invalid output_path must be rejected before the remote call")
		})
	}
}

func TestCallTool_MultiItemResultSpilledAsJSON(t *testing.T) {
	m, ws, _ := newSpillManager(t, 10, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{
			{Type: "text", Text: strings.Repeat("a", 200)},
			{Type: "text", Text: strings.Repeat("b", 200)},
		},
	})
	defer m.Close()

	out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "")
	require.NoError(t, err)
	env, ok := out.(spillEnvelope)
	require.True(t, ok)
	assert.True(t, strings.HasSuffix(env.Path, ".json"), "multi-item payload uses the JSON form")
	assert.Equal(t, 2, env.Items)

	var items []mcpclient.Content
	data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(env.Path)))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &items))
	require.Len(t, items, 2)
	assert.Equal(t, strings.Repeat("a", 200), items[0].Text)
}

func TestCallTool_SpillWriteFailureDegradesToTruncated(t *testing.T) {
	// Payload must exceed the 1KB preview window so the degradation actually
	// withholds bytes from the model.
	m, ws, _ := newSpillManager(t, 10, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{bigText(200)},
	})
	defer m.Close()

	// Force the automatic-spill write to fail: a regular FILE occupies the
	// tmp/mcp-outputs path, so the spill's MkdirAll fails.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "tmp"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "tmp", "mcp-outputs"), []byte("i am a file"), 0o644))

	out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "")
	require.NoError(t, err, "the remote call succeeded; degradation is not an error")
	env, ok := out.(truncatedEnvelope)
	require.True(t, ok, "write failure must degrade to truncatedEnvelope, got %T", out)
	assert.True(t, env.Truncated)
	assert.Equal(t, 200*8, env.Size)
	assert.Contains(t, env.Note, "could not be saved")
	assert.Less(t, len(env.Preview), 200*8, "full payload must never come back inline")
}

func TestSpill_ParallelAndCrossTurnUniqueness(t *testing.T) {
	m, _, _ := newSpillManager(t, 10, &mcpclient.ToolsCallResult{
		Content: []mcpclient.Content{bigText(10)},
	})
	defer m.Close()

	// Parallel dispatches within one round (same manager/trace) get distinct
	// seq numbers — no overwrite, both files exist.
	var wg sync.WaitGroup
	paths := make([]string, 8)
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := callTool(context.Background(), m, "gildata", "query", json.RawMessage(`{}`), "")
			require.NoError(t, err)
			env, ok := out.(spillEnvelope)
			require.True(t, ok)
			paths[i] = env.Path
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range paths {
		assert.False(t, seen[p], "duplicate spill path %q", p)
		seen[p] = true
	}

	// Cross-turn: a fresh manager (new trace-less ctx, seq restarts at 1)
	// must still not collide because the trace id differs — pin the naming
	// contract directly.
	a := autoSpillRelPath("gildata", "query", "trace-a", 1, ".txt")
	b := autoSpillRelPath("gildata", "query", "trace-b", 1, ".txt")
	assert.NotEqual(t, a, b, "same seq across turns must diverge via trace id")
	assert.Equal(t, "tmp/mcp-outputs/gildata/query-trace-a-1.txt", a)
}

func TestSpill_AutoPathSanitizesSegments(t *testing.T) {
	// A hostile tool name with separators must not escape the spill dir:
	// unsafe runes collapse per-segment, so every joined segment is a
	// literal name (never "." or "..").
	p := autoSpillRelPath("../../etc", "pwn/../../sh", "t-1", 1, ".txt")
	assert.True(t, strings.HasPrefix(p, "tmp/mcp-outputs/"), "path %q must stay under the spill dir", p)
	for _, seg := range strings.Split(p, "/") {
		assert.NotContains(t, []string{".", "..", ""}, seg, "segment %q must be a literal name", seg)
	}
	// Empty trace falls back to a uniqueness-preserving stamp.
	assert.Contains(t, autoSpillRelPath("s", "t", "", 1, ".txt"), "-t1")
}

func TestSpill_PreviewRuneSafe(t *testing.T) {
	// Exactly at the limit: no truncation marker.
	assert.Equal(t, "abc", previewOf([]byte("abc")))
	// A multi-byte rune straddling the 1024-byte cut must not be split.
	data := []byte(strings.Repeat("a", 1022) + "你好世界")
	got := previewOf(data)
	assert.True(t, strings.HasSuffix(got, "…[truncated]"))
	assert.NotContains(t, got[strings.Index(got, "…"):], "�", "rune must not be split")
	assert.LessOrEqual(t, len(got), previewBytes+len("…[truncated]"))
}

func TestSpill_SanitizeSegment(t *testing.T) {
	assert.Equal(t, "abc-1_.", sanitizeSegment("abc-1_."))
	assert.Equal(t, "a_b", sanitizeSegment("a/b"))
	assert.Equal(t, "x", sanitizeSegment(""))
	assert.Equal(t, "x", sanitizeSegment(".."))
	assert.Equal(t, "___", sanitizeSegment("***"), "unsafe runes collapse to '_', keeping length")
}
