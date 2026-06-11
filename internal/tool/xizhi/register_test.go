package xizhi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterAll_RegistersThreeTools(t *testing.T) {
	r := newTestRegistry(t)
	RegisterAll(r, t.TempDir())

	for _, name := range []string{NameReadFile, NameWriteFile, NameModifyFile} {
		spec, ok := r.Get(name)
		require.True(t, ok, "tool %q missing", name)
		assert.NotEmpty(t, spec.Description)
		assert.NotEmpty(t, spec.ParametersJSON)
		assert.NotNil(t, spec.Execute)
	}
}

func TestRegisterAll_PanicsOnDuplicate(t *testing.T) {
	r := newTestRegistry(t)
	RegisterAll(r, t.TempDir())
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterAll twice did not panic")
		}
	}()
	RegisterAll(r, t.TempDir())
}

func TestRegisterAll_SchemasAreValidJSON(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"read":   schemaRead,
		"write":  schemaWrite,
		"modify": schemaModify,
	} {
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s schema is not valid JSON: %v", name, err)
		}
		if decoded["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", name, decoded["type"])
		}
	}
}

func TestRegisterAll_ReadExecute_DecodesArgs(t *testing.T) {
	root := t.TempDir()
	r := newTestRegistry(t)
	RegisterAll(r, root)

	spec, ok := r.Get(NameReadFile)
	require.True(t, ok)

	// Reading a non-existent file exercises arg decoding and reaches ReadFile.
	args, err := json.Marshal(readArgs{Path: "missing.txt"})
	require.NoError(t, err)

	_, err = spec.Execute(context.Background(), args)
	assert.Error(t, err)
}
