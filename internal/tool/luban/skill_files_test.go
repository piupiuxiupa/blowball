package luban

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/tool/skill"
)

// newSkillFileLoader builds a loader over a global dir and a per-user dir under
// dataDir, mirroring the production wiring.
func newSkillFileLoader(t *testing.T) (loader *skill.Loader, globalDir, userDataDir string, userDirFn func(userID string) string) {
	t.Helper()
	globalDir = t.TempDir()
	userDataDir = t.TempDir()
	userDirFn = func(userID string) string { return filepath.Join(userDataDir, userID, "skills") }
	return skill.NewLoader(globalDir, userDirFn), globalDir, userDataDir, userDirFn
}

// seedSkill populates a skill directory with a deterministic layout used by the
// list/tree tests: README.md, SKILL.md, examples/guide.md, templates/tmpl.yaml,
// and a hidden .git/config (so hidden-by-default and path-traversal scenarios
// have something to assert against).
func seedSkill(t *testing.T, skillDir string) {
	t.Helper()
	writeSkill(t, skillDir, "my-skill", "My", "# My")
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "README.md"), []byte("readme"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "examples"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "examples", "guide.md"), []byte("guide"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "templates", "tmpl.yaml"), []byte("tmpl"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, ".git", "config"), []byte("cfg"), 0o644))
}

func entryNames(entries []skillFileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func nodeNames(nodes []skillTreeNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestListSkillFiles_Root(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := ListSkillFiles(loader, "my-skill", "", "u1", false)
	require.NoError(t, err)
	got := res.(listSkillFilesResult)
	assert.Equal(t, ".", got.Path, "empty path displays as \".\"")
	// Hidden .git excluded by default; entries sorted by name.
	assert.Equal(t, []string{"README.md", "SKILL.md", "examples", "templates"}, entryNames(got.Entries))

	// examples/templates are dirs; README.md/SKILL.md are files with sizes.
	byName := map[string]skillFileEntry{}
	for _, e := range got.Entries {
		byName[e.Name] = e
	}
	assert.Equal(t, "dir", byName["examples"].Type)
	assert.Equal(t, "file", byName["README.md"].Type)
	assert.Equal(t, int64(len("readme")), byName["README.md"].Size)
}

func TestListSkillFiles_SubDirectory(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := ListSkillFiles(loader, "my-skill", "examples", "u1", false)
	require.NoError(t, err)
	got := res.(listSkillFilesResult)
	assert.Equal(t, "examples", got.Path)
	assert.Equal(t, []string{"guide.md"}, entryNames(got.Entries))
}

func TestListSkillFiles_UserOverridesGlobal(t *testing.T) {
	loader, globalDir, _, userDirFn := newSkillFileLoader(t)
	// Global "shared" has a marker file "global-marker"; user "shared" has
	// "user-marker". The listing must reflect the user directory.
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "shared"), 0o755))
	writeSkill(t, filepath.Join(globalDir, "shared"), "shared", "Global", "# Global")
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "shared", "global-marker"), []byte("g"), 0o644))

	require.NoError(t, os.MkdirAll(userDirFn("u1"), 0o755))
	writeSkill(t, filepath.Join(userDirFn("u1"), "shared"), "shared", "User", "# User")
	require.NoError(t, os.WriteFile(filepath.Join(userDirFn("u1"), "shared", "user-marker"), []byte("u"), 0o644))

	res, err := ListSkillFiles(loader, "shared", "", "u1", false)
	require.NoError(t, err)
	got := res.(listSkillFilesResult)
	assert.Contains(t, entryNames(got.Entries), "user-marker", "user skill directory must be the one listed")
	assert.NotContains(t, entryNames(got.Entries), "global-marker", "global skill of same name must be shadowed")
}

func TestListSkillFiles_HiddenGitExcludedByDefault(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	// Default: .git hidden.
	res, err := ListSkillFiles(loader, "my-skill", "", "u1", false)
	require.NoError(t, err)
	assert.NotContains(t, entryNames(res.(listSkillFilesResult).Entries), ".git")

	// include_hidden surfaces it.
	res, err = ListSkillFiles(loader, "my-skill", "", "u1", true)
	require.NoError(t, err)
	assert.Contains(t, entryNames(res.(listSkillFilesResult).Entries), ".git")
}

func TestListSkillFiles_PathTraversalRejected(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	_, err := ListSkillFiles(loader, "my-skill", "../../etc", "u1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")
}

func TestListSkillFiles_UnknownSkill(t *testing.T) {
	loader, _, _, _ := newSkillFileLoader(t)
	_, err := ListSkillFiles(loader, "nonexistent", "", "u1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestListSkillFiles_SubDirectoryNotFound(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	_, err := ListSkillFiles(loader, "my-skill", "nope", "u1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found")
}

func TestTreeSkill_RootDefaultDepth(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := TreeSkill(loader, "my-skill", "", "u1", 0, false)
	require.NoError(t, err)
	got := res.(treeSkillResult)
	assert.Equal(t, ".", got.Path)
	assert.Equal(t, defaultSkillTreeDepth, got.Depth, "zero depth falls back to the default")
	// Hidden .git excluded; top-level nodes sorted by name.
	assert.Equal(t, []string{"README.md", "SKILL.md", "examples", "templates"}, nodeNames(got.Tree))

	// examples dir recurses one level: guide.md.
	var examples *skillTreeNode
	for i := range got.Tree {
		if got.Tree[i].Name == "examples" {
			examples = &got.Tree[i]
		}
	}
	require.NotNil(t, examples)
	assert.Equal(t, "dir", examples.Type)
	assert.Equal(t, []string{"guide.md"}, nodeNames(examples.Children))
}

func TestTreeSkill_DepthClampedToMax(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := TreeSkill(loader, "my-skill", "", "u1", 42, false)
	require.NoError(t, err)
	got := res.(treeSkillResult)
	assert.Equal(t, maxSkillTreeDepth, got.Depth, "depth above the maximum must be clamped to 10")
}

func TestTreeSkill_SubDirectory(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := TreeSkill(loader, "my-skill", "examples", "u1", 2, false)
	require.NoError(t, err)
	got := res.(treeSkillResult)
	assert.Equal(t, "examples", got.Path)
	assert.Equal(t, 2, got.Depth)
	assert.Equal(t, []string{"guide.md"}, nodeNames(got.Tree))
}

func TestTreeSkill_HiddenExcludedByDefault(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	res, err := TreeSkill(loader, "my-skill", "", "u1", 3, false)
	require.NoError(t, err)
	assert.NotContains(t, nodeNames(res.(treeSkillResult).Tree), ".git")

	res, err = TreeSkill(loader, "my-skill", "", "u1", 3, true)
	require.NoError(t, err)
	assert.Contains(t, nodeNames(res.(treeSkillResult).Tree), ".git")
}

func TestTreeSkill_PathTraversalRejected(t *testing.T) {
	loader, globalDir, _, _ := newSkillFileLoader(t)
	seedSkill(t, filepath.Join(globalDir, "my-skill"))

	_, err := TreeSkill(loader, "my-skill", "../../etc", "u1", 3, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "traversal")
}

func TestTreeSkill_UnknownSkill(t *testing.T) {
	loader, _, _, _ := newSkillFileLoader(t)
	_, err := TreeSkill(loader, "nonexistent", "", "u1", 3, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
