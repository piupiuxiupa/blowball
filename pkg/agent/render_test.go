package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envelopeShape decodes the rendered envelope so tests can assert on the
// individual status/result/error fields without depending on exact byte order.
type envelopeShape struct {
	Status int             `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func decodeEnvelope(t *testing.T, rendered string) envelopeShape {
	t.Helper()
	var env envelopeShape
	require.NoError(t, json.Unmarshal([]byte(rendered), &env), "rendered output must be valid JSON: %q", rendered)
	return env
}

func TestRenderToolResult_ObjectSuccess(t *testing.T) {
	out := map[string]any{"path": "src", "entries": []string{"a.go"}}
	env := decodeEnvelope(t, renderToolResult(out, nil))
	assert.Equal(t, 0, env.Status)
	assert.Empty(t, env.Error, "success must omit error")

	var got map[string]any
	require.NoError(t, json.Unmarshal(env.Result, &got))
	assert.Equal(t, "src", got["path"])
}

func TestRenderToolResult_ArraySuccess(t *testing.T) {
	out := []map[string]any{{"name": "x"}, {"name": "y"}}
	env := decodeEnvelope(t, renderToolResult(out, nil))
	assert.Equal(t, 0, env.Status)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(env.Result, &got))
	require.Len(t, got, 2)
	assert.Equal(t, "x", got[0]["name"])
}

func TestRenderToolResult_BareStringSuccess(t *testing.T) {
	env := decodeEnvelope(t, renderToolResult("hello world", nil))
	assert.Equal(t, 0, env.Status)
	// A bare string return nests under result as a JSON string.
	var got string
	require.NoError(t, json.Unmarshal(env.Result, &got))
	assert.Equal(t, "hello world", got)
}

func TestRenderToolResult_ByteSliceNotBase64(t *testing.T) {
	// A []byte return MUST be emitted as text, not base64 (preserving the prior
	// marshalToolResult semantics).
	rendered := renderToolResult([]byte("plain text body"), nil)
	env := decodeEnvelope(t, rendered)
	assert.Equal(t, 0, env.Status)

	var got string
	require.NoError(t, json.Unmarshal(env.Result, &got))
	assert.Equal(t, "plain text body", got)

	// Defense-in-depth: the decoded text must not be a base64 re-decoding of the
	// original bytes.
	_, decErr := base64.StdEncoding.DecodeString("plain text body")
	assert.Error(t, decErr, "sanity: the raw bytes are not base64 to begin with")
	assert.NotContains(t, string(env.Result), base64.StdEncoding.EncodeToString([]byte("plain text body")),
		"result must not carry a base64 encoding of the byte slice")
}

func TestRenderToolResult_NilSuccess(t *testing.T) {
	env := decodeEnvelope(t, renderToolResult(nil, nil))
	assert.Equal(t, 0, env.Status)
	assert.Empty(t, env.Error)
	// nil out → result:null
	assert.Equal(t, "null", string(env.Result))
}

func TestRenderToolResult_Error(t *testing.T) {
	env := decodeEnvelope(t, renderToolResult(nil, errors.New("file not found")))
	assert.Equal(t, 1, env.Status)
	assert.Equal(t, "file not found", env.Error)
	// Failure must omit result. The omitempty on a zero-length RawMessage drops
	// the field entirely, so the raw key is absent from the rendered object.
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(renderToolResult(nil, errors.New("boom"))), &raw))
	_, hasResult := raw["result"]
	assert.False(t, hasResult, "failure envelope must not carry a result field")
	assert.Equal(t, "boom", raw["error"])
}
