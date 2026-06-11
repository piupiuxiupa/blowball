package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSpec(name string) *ToolSpec {
	return &ToolSpec{
		Name:           name,
		Description:    name + " description",
		ParametersJSON: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"name": name}, nil
		},
	}
}

func TestRegister_AddsToRegistry(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("a")))

	spec, ok := r.Get("a")
	require.True(t, ok)
	assert.Equal(t, "a", spec.Name)
}

func TestRegister_DuplicateName_Errors(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("a")))
	err := r.Register(newSpec("a"))
	if err == nil {
		t.Fatal("duplicate Register returned nil error")
	}
	if !errors.Is(err, err) { // sentinel isn't required; ensure err is non-nil and human-readable
		t.Fatalf("err is unexpected: %v", err)
	}
	assert.Contains(t, err.Error(), "already registered")
}

func TestRegister_EmptyName_Errors(t *testing.T) {
	r := NewRegistry()
	spec := newSpec("")
	err := r.Register(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestRegister_NilExecute_Errors(t *testing.T) {
	r := NewRegistry()
	spec := newSpec("x")
	spec.Execute = nil
	err := r.Register(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil Execute")
}

func TestMustGet_PanicsWhenMissing(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet on missing tool did not panic")
		}
	}()
	_ = r.MustGet("ghost")
}

func TestMustGet_ReturnsSpecWhenPresent(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("a")))
	spec := r.MustGet("a")
	assert.Equal(t, "a", spec.Name)
}

func TestList_SortedByName(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		require.NoError(t, r.Register(newSpec(n)))
	}
	got := r.List()
	require.Len(t, got, 3)
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "b", got[1].Name)
	assert.Equal(t, "c", got[2].Name)
}

func TestToolsFor_ReturnsInOrder(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		require.NoError(t, r.Register(newSpec(n)))
	}
	specs, err := r.ToolsFor([]string{"c", "a", "b"})
	require.NoError(t, err)
	require.Len(t, specs, 3)
	assert.Equal(t, "c", specs[0].Name)
	assert.Equal(t, "a", specs[1].Name)
	assert.Equal(t, "b", specs[2].Name)
}

func TestToolsFor_MissingName_ErrorsListsMissing(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("a")))

	_, err := r.ToolsFor([]string{"a", "ghost1", "ghost2"})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "ghost1")
	assert.Contains(t, msg, "ghost2")
}

func TestCall_Dispatches(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("a")))

	res, err := r.Call(context.Background(), "a", json.RawMessage(`{}`))
	require.NoError(t, err)
	m, ok := res.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", m["name"])
}

func TestCall_UnknownTool_Errors(t *testing.T) {
	r := NewRegistry()
	_, err := r.Call(context.Background(), "ghost", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")
}

func TestOpenAITools_ProducesExpectedShape(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("alpha")))
	require.NoError(t, r.Register(newSpec("beta")))

	raw, err := r.OpenAITools([]string{"alpha"})
	require.NoError(t, err)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, 1)

	entry := got[0]
	assert.Equal(t, "function", entry["type"])

	fn, ok := entry["function"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alpha", fn["name"])
	assert.Equal(t, "alpha description", fn["description"])

	// parameters must be the schema object verbatim.
	params, ok := fn["parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", params["type"])
}

func TestOpenAITools_AllNamesRequested(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("alpha")))
	require.NoError(t, r.Register(newSpec("beta")))

	raw, err := r.OpenAITools([]string{"beta", "alpha"})
	require.NoError(t, err)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, 2)
	assert.Equal(t, "beta", got[0]["function"].(map[string]any)["name"])
	assert.Equal(t, "alpha", got[1]["function"].(map[string]any)["name"])
}

func TestOpenAITools_MissingName_Errors(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(newSpec("alpha")))

	_, err := r.OpenAITools([]string{"alpha", "ghost"})
	require.Error(t, err)
}

func TestOpenAITools_EmptyParametersDefaults(t *testing.T) {
	r := NewRegistry()
	spec := newSpec("empty")
	spec.ParametersJSON = nil
	require.NoError(t, r.Register(spec))

	raw, err := r.OpenAITools([]string{"empty"})
	require.NoError(t, err)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	fn := got[0]["function"].(map[string]any)
	params := fn["parameters"]
	// Default schema should be an empty object, not null.
	assert.Equal(t, map[string]any{}, params)
}
