package skill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoader_Discover_GlobalAndUser(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "u1", "skills"), 0o755))

	writeSkill(t, filepath.Join(globalDir, "coding-style"), "coding-style", "Global coding style", "# Global")
	writeSkill(t, filepath.Join(userDirFn("u1"), "coding-style"), "coding-style", "User coding style", "# User")
	writeSkill(t, filepath.Join(globalDir, "review"), "review", "Review skill", "# Review")

	loader := NewLoader(globalDir, userDirFn)

	globalOnly := loader.List("")
	require.Len(t, globalOnly, 2)
	names := make([]string, len(globalOnly))
	for i, s := range globalOnly {
		names[i] = s.Name
	}
	assert.Equal(t, []string{"coding-style", "review"}, names)

	user := loader.List("u1")
	require.Len(t, user, 2)
	descriptions := make(map[string]string)
	for _, s := range user {
		descriptions[s.Name] = s.Description
	}
	assert.Equal(t, "User coding style", descriptions["coding-style"])
	assert.Equal(t, "Review skill", descriptions["review"])
}

func TestLoader_Read_StripsFrontmatter(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "coding-style"), "coding-style", "Coding style", "# Body\n\nUse gofmt.")

	loader := NewLoader(globalDir, nil)
	body, err := loader.Read("coding-style", "")
	require.NoError(t, err)
	assert.Equal(t, "# Body\n\nUse gofmt.", string(body))
}

func TestLoader_Read_UserOverridesGlobal(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "Global", "# Global")
	writeSkill(t, filepath.Join(userDirFn("u1"), "s"), "s", "User", "# User")

	loader := NewLoader(globalDir, userDirFn)
	body, err := loader.Read("s", "u1")
	require.NoError(t, err)
	assert.Equal(t, "# User", string(body))
}

func TestLoader_Read_Unknown(t *testing.T) {
	loader := NewLoader(t.TempDir(), nil)
	_, err := loader.Read("missing", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoader_Read_SizeLimit(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "big"), "big", "Big", "hello")

	loader := NewLoader(globalDir, nil).WithMaxSize(2)
	_, err := loader.Read("big", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds size limit")
}

func TestLoader_Read_MissingDescription(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "bad"), "bad", "", "# Body")

	loader := NewLoader(globalDir, nil)
	assert.Empty(t, loader.List(""))
}

func TestLoader_HasSkill(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "S", "# Body")

	loader := NewLoader(globalDir, nil)
	assert.True(t, loader.HasSkill("s", ""))
	assert.False(t, loader.HasSkill("missing", ""))
}

func TestLoader_ListGlobal(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))

	writeSkill(t, filepath.Join(globalDir, "coding-style"), "coding-style", "Global", "# Global")
	writeSkill(t, filepath.Join(userDirFn("u1"), "coding-style"), "coding-style", "User", "# User")

	loader := NewLoader(globalDir, userDirFn)
	global := loader.ListGlobal()
	require.Len(t, global, 1)
	assert.Equal(t, "coding-style", global[0].Name)
	assert.Equal(t, "Global", global[0].Description)
	assert.Equal(t, "global", global[0].Location)
}

func TestLoader_Discover_Recursive(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "collection", "skills", "nested"), "nested", "Nested skill", "# Nested")
	writeSkill(t, filepath.Join(globalDir, "shallow"), "shallow", "Shallow skill", "# Shallow")

	loader := NewLoader(globalDir, nil)
	skills := loader.ListGlobal()
	require.Len(t, skills, 2)
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	assert.ElementsMatch(t, []string{"nested", "shallow"}, names)

	// Verify the nested path is recorded correctly.
	var nested Skill
	for _, s := range skills {
		if s.Name == "nested" {
			nested = s
			break
		}
	}
	assert.Contains(t, nested.Path, filepath.Join("collection", "skills", "nested", "SKILL.md"))
}

func TestLoader_Discover_UserOverridesGlobal_Recursive(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string {
		return filepath.Join(dataDir, userID, "skills")
	}
	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))

	writeSkill(t, filepath.Join(globalDir, "collection", "skills", "s"), "s", "Global", "# Global")
	writeSkill(t, filepath.Join(userDirFn("u1"), "s"), "s", "User", "# User")

	loader := NewLoader(globalDir, userDirFn)
	body, err := loader.Read("s", "u1")
	require.NoError(t, err)
	assert.Equal(t, "# User", string(body))

	global := loader.ListGlobal()
	require.Len(t, global, 1)
	assert.Equal(t, "global", global[0].Location)
}

func TestLoader_ReadPath_NestedSubDocument(t *testing.T) {
	globalDir := t.TempDir()
	skillDir := filepath.Join(globalDir, "my-skill")
	writeSkill(t, skillDir, "my-skill", "My skill", "# Skill")
	// Nested sub-document under the skill directory.
	subDir := filepath.Join(skillDir, "references")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "api.md"), []byte("# API reference"), 0o644))

	loader := NewLoader(globalDir, nil)
	body, err := loader.ReadPath("my-skill", "references/api.md", "")
	require.NoError(t, err)
	assert.Equal(t, "# API reference", string(body))
}

func TestLoader_ReadPath_StripsFrontmatter(t *testing.T) {
	globalDir := t.TempDir()
	skillDir := filepath.Join(globalDir, "my-skill")
	writeSkill(t, skillDir, "my-skill", "My skill", "# Skill")
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "guide.md"),
		[]byte("---\nauthor: someone\n---\n# Guide body"), 0o644))

	loader := NewLoader(globalDir, nil)
	body, err := loader.ReadPath("my-skill", "guide.md", "")
	require.NoError(t, err)
	assert.Equal(t, "# Guide body", string(body))
}

func TestLoader_ReadPath_RejectsAbsolute(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "S", "# Body")

	loader := NewLoader(globalDir, nil)
	_, err := loader.ReadPath("s", "/etc/passwd", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute paths are not allowed")
}

func TestLoader_ReadPath_RejectsParentTraversal(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "S", "# Body")

	loader := NewLoader(globalDir, nil)
	_, err := loader.ReadPath("s", "../../shared.md", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path traversal rejected")
}

func TestLoader_ReadPath_RejectsSymlinkEscape(t *testing.T) {
	globalDir := t.TempDir()
	skillDir := filepath.Join(globalDir, "s")
	writeSkill(t, skillDir, "s", "S", "# Body")
	// A file OUTSIDE the skill directory (but inside globalDir).
	outside := filepath.Join(globalDir, "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	// A symlink inside the skill dir whose target escapes it.
	require.NoError(t, os.Symlink(outside, filepath.Join(skillDir, "evil.md")))

	loader := NewLoader(globalDir, nil)
	_, err := loader.ReadPath("s", "evil.md", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the skill directory")
}

func TestLoader_ReadPath_RejectsNonMarkdown(t *testing.T) {
	globalDir := t.TempDir()
	skillDir := filepath.Join(globalDir, "s")
	writeSkill(t, skillDir, "s", "S", "# Body")
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "data.csv"), []byte("a,b"), 0o644))

	loader := NewLoader(globalDir, nil)
	_, err := loader.ReadPath("s", "data.csv", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not markdown")
}

func TestLoader_ReadPath_RejectsOversized(t *testing.T) {
	globalDir := t.TempDir()
	skillDir := filepath.Join(globalDir, "s")
	writeSkill(t, skillDir, "s", "S", "# Body")
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "big.md"), []byte("hello"), 0o644))

	loader := NewLoader(globalDir, nil).WithMaxSize(2)
	_, err := loader.ReadPath("s", "big.md", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds size limit")
}

func TestLoader_ReadPath_MissingFile(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, filepath.Join(globalDir, "s"), "s", "S", "# Body")

	loader := NewLoader(globalDir, nil)
	_, err := loader.ReadPath("s", "nope.md", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file not found")
}

func TestLoader_ReadPath_UnknownSkill(t *testing.T) {
	loader := NewLoader(t.TempDir(), nil)
	_, err := loader.ReadPath("missing", "guide.md", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestFilter(t *testing.T) {
	skills := []Skill{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}
	filtered := Filter(skills, []string{"b", "d"})
	require.Len(t, filtered, 1)
	assert.Equal(t, "b", filtered[0].Name)
}

func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "---\nname: " + name + "\n"
	if description != "" {
		content += "description: " + description + "\n"
	}
	content += "---\n" + body
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}
