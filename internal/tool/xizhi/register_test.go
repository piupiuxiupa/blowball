package xizhi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
)

func TestRegisterAll_RegistersEnabledTools(t *testing.T) {
	r := newTestRegistry(t)
	RegisterAll(r, t.TempDir(), testXizhiConfig())

	for _, name := range []string{NameReadFile, NameWriteFile, NameModifyFile, NameListFiles, NameTree, NameGlobFiles} {
		spec, ok := r.Get(name)
		require.True(t, ok, "tool %q missing", name)
		assert.NotEmpty(t, spec.Description)
		assert.NotEmpty(t, spec.ParametersJSON)
		assert.NotNil(t, spec.Execute)
	}
}

func TestRegisterAll_PanicsOnDuplicate(t *testing.T) {
	r := newTestRegistry(t)
	RegisterAll(r, t.TempDir(), testXizhiConfig())
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterAll twice did not panic")
		}
	}()
	RegisterAll(r, t.TempDir(), testXizhiConfig())
}

func TestRegisterAll_SchemasAreValidJSON(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"read":       schemaRead,
		"write":      schemaWrite,
		"modify":     schemaModify,
		"list":       schemaList,
		"tree":       schemaTree,
		"glob":       schemaGlob,
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
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameReadFile)
	require.True(t, ok)

	// Reading a non-existent file exercises arg decoding and reaches ReadFile.
	args, err := json.Marshal(readArgs{Path: "missing.txt"})
	require.NoError(t, err)

	_, err = spec.Execute(context.Background(), args)
	assert.Error(t, err)
}

func TestRegisterAll_RespectsEnabledFlags(t *testing.T) {
	r := newTestRegistry(t)
	cfg := config.XizhiConfig{
		Read:      config.XizhiToolConfig{Enabled: false},
		Write:     config.XizhiToolConfig{Enabled: false},
		Modify:    config.XizhiToolConfig{Enabled: false},
		ListFiles: config.XizhiToolConfig{Enabled: true},
		Tree:      config.XizhiToolConfig{Enabled: false},
		GlobFiles: config.XizhiToolConfig{Enabled: true},
	}
	RegisterAll(r, t.TempDir(), cfg)

	// Original tools are always registered.
	_, ok := r.Get(NameReadFile)
	assert.True(t, ok, "read should be registered")
	_, ok = r.Get(NameWriteFile)
	assert.True(t, ok, "write should be registered")
	_, ok = r.Get(NameModifyFile)
	assert.True(t, ok, "modify should be registered")

	// New tools respect the enabled flag.
	_, ok = r.Get(NameListFiles)
	assert.True(t, ok, "list_files should be registered")
	_, ok = r.Get(NameTree)
	assert.False(t, ok, "tree should not be registered")
	_, ok = r.Get(NameGlobFiles)
	assert.True(t, ok, "glob_files should be registered")
}
