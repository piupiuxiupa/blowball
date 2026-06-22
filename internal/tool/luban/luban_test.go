package luban

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

func TestValidateSkillName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"", false},
		{"../etc/passwd", false},
		{"a/../b", false},
		{"a/b", false},
		{"a\\b", false},
		{"good-name", true},
		{"good_name", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSkillName(tc.name)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestListSkills(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))

	writeSkill(t, filepath.Join(globalDir, "global-skill"), "global-skill", "Global", "# Global")
	writeSkill(t, filepath.Join(globalDir, "collection", "skills", "nested"), "nested", "Nested", "# Nested")
	writeSkill(t, filepath.Join(userDirFn("u1"), "user-skill"), "user-skill", "User", "# User")
	writeSkill(t, filepath.Join(userDirFn("u1"), "global-skill"), "global-skill", "Override", "# Override")

	loader := skill.NewLoader(globalDir, userDirFn)

	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	require.Len(t, entries, 3)

	desc := make(map[string]string)
	loc := make(map[string]string)
	for _, e := range entries {
		desc[e.Name] = e.Description
		loc[e.Name] = e.Location
	}
	assert.Equal(t, "Override", desc["global-skill"])
	assert.Equal(t, "Nested", desc["nested"])
	assert.Equal(t, "User", desc["user-skill"])
	assert.Equal(t, "user", loc["global-skill"])
	assert.Equal(t, "global", loc["nested"])
	assert.Equal(t, "user", loc["user-skill"])
}

func TestReadSkill(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))

	writeSkill(t, filepath.Join(globalDir, "s"), "s", "Global", "# Global")
	writeSkill(t, filepath.Join(userDirFn("u1"), "s"), "s", "User", "# User")

	loader := skill.NewLoader(globalDir, userDirFn)

	body, err := readSkill(loader, "s", "u1")
	require.NoError(t, err)
	assert.Equal(t, "# User", body)

	body, err = readSkill(loader, "s", "")
	require.NoError(t, err)
	assert.Equal(t, "# Global", body)

	_, err = readSkill(loader, "missing", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestReadSkill_RejectsPathLikeName(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), nil)
	_, err := readSkill(loader, "../workspace/secrets", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "luban_read_skill")
}

func TestInstallSkill_SingleFile(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "---\nname: from-url\ndescription: From URL\n---\n# Body")
	}))
	defer server.Close()

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.httpClient = server.Client()

	res, err := ins.installSkill(context.Background(), server.URL+"/skill.md", "", "u1")
	require.NoError(t, err)
	assert.Equal(t, "from-url", res.Name)
	assert.False(t, res.Overwrite)

	path := filepath.Join(userDirFn("u1"), "from-url", "SKILL.md")
	assert.FileExists(t, path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# Body")

	// Listing should discover the newly installed skill.
	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "from-url", entries[0].Name)
}

func TestInstallSkill_InvalidURL(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	_, err := ins.installSkill(context.Background(), "http://example.com/repo", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid URL")

	_, err = ins.installSkill(context.Background(), "not-a-url", "", "u1")
	require.Error(t, err)
}

func TestInstallSkill_PathTraversalName(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	_, err := ins.installSkill(context.Background(), "https://example.com/repo", "../escape", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "luban_install_skill")
}

func TestInstallSkill_NoUserID(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	_, err := ins.installSkill(context.Background(), "https://example.com/repo", "name", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no userID")
}

func TestInstallSkill_GitRepo(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)

	// Simulate a git clone by writing the expected directory tree.
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "skills", "sub-skill"), 0o755))
		writeSkill(t, filepath.Join(targetDir, "skills", "sub-skill"), "sub-skill", "Sub", "# Sub")
		return nil
	}

	res, err := ins.installSkill(context.Background(), "https://example.com/collection", "collection", "u1")
	require.NoError(t, err)
	assert.Equal(t, "collection", res.Name)
	assert.False(t, res.Overwrite)

	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "sub-skill", entries[0].Name)
}

func TestRegisterAll(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	tools := NewTools(loader, func(string) string { return t.TempDir() })
	r := tool.NewRegistry()
	require.NoError(t, RegisterAll(r, tools))

	for _, name := range []string{ToolListSkills, ToolReadSkill, ToolInstallSkill} {
		_, ok := r.Get(name)
		assert.True(t, ok, name)
	}
}

func TestReadSkillTool_Execute(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "S", "# Skill")

	loader := skill.NewLoader(globalDir, nil)
	tools := NewTools(loader, func(string) string { return t.TempDir() })
	r := tool.NewRegistry()
	require.NoError(t, RegisterAll(r, tools))

	ctx := skill.WithUserID(context.Background(), "")
	out, err := r.Call(ctx, ToolReadSkill, json.RawMessage(`{"name":"s"}`))
	require.NoError(t, err)
	assert.Equal(t, "# Skill", out)
}

func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, description, body)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}
