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

	res, err := ins.installSkill(context.Background(), server.URL+"/skill.md", "", "", "u1")
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

	_, err := ins.installSkill(context.Background(), "http://example.com/repo", "", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid URL")

	_, err = ins.installSkill(context.Background(), "not-a-url", "", "", "u1")
	require.Error(t, err)
}

func TestInstallSkill_PathTraversalName(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	_, err := ins.installSkill(context.Background(), "https://example.com/repo", "../escape", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "luban_install_skill")
}

func TestInstallSkill_NoUserID(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	_, err := ins.installSkill(context.Background(), "https://example.com/repo", "name", "", "")
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

	res, err := ins.installSkill(context.Background(), "https://example.com/collection", "collection", "", "u1")
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

// collectionClone writes a collection repo with two sub-skills into targetDir,
// simulating what a `git clone` of a multi-skill repository would produce.
func collectionClone(t *testing.T, targetDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "skills", "gildata-finance-data"), 0o755))
	writeSkill(t, filepath.Join(targetDir, "skills", "gildata-finance-data"), "gildata-finance-data", "Finance data skill", "# Finance")
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "skills", "wind-data"), 0o755))
	writeSkill(t, filepath.Join(targetDir, "skills", "wind-data"), "wind-data", "Wind data skill", "# Wind")
}

func TestInstallSkill_SubSkill_ByName(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		collectionClone(t, targetDir)
		return nil
	}

	res, err := ins.installSkill(context.Background(), "https://example.com/collection", "", "gildata-finance-data", "u1")
	require.NoError(t, err)
	assert.True(t, res.Installed)
	assert.Equal(t, "install", res.Kind)
	assert.Equal(t, "gildata-finance-data", res.Name)

	// Only the selected sub-skill is present; the rest of the clone is discarded.
	assert.FileExists(t, filepath.Join(userDirFn("u1"), "gildata-finance-data", "SKILL.md"))
	assert.NoDirExists(t, filepath.Join(userDirFn("u1"), "wind-data"))

	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "gildata-finance-data", entries[0].Name)
}

func TestInstallSkill_SubSkill_BySubpath(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		collectionClone(t, targetDir)
		return nil
	}

	// A subpath that does not equal any frontmatter name exercises the fallback.
	res, err := ins.installSkill(context.Background(), "https://example.com/collection", "", "skills/wind-data", "u1")
	require.NoError(t, err)
	assert.True(t, res.Installed)
	// Installed name is the selected sub-skill's frontmatter name.
	assert.Equal(t, "wind-data", res.Name)
	assert.FileExists(t, filepath.Join(userDirFn("u1"), "wind-data", "SKILL.md"))
	assert.NoDirExists(t, filepath.Join(userDirFn("u1"), "gildata-finance-data"))
}

func TestInstallSkill_SubSkill_NotFoundListsNames(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		collectionClone(t, targetDir)
		return nil
	}

	_, err := ins.installSkill(context.Background(), "https://example.com/collection", "", "does-not-exist", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "gildata-finance-data")
	assert.Contains(t, err.Error(), "wind-data")

	// Nothing is written to the user skills directory.
	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestInstallSkill_SubSkill_NameOverride(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		collectionClone(t, targetDir)
		return nil
	}

	res, err := ins.installSkill(context.Background(), "https://example.com/collection", "my-name", "gildata-finance-data", "u1")
	require.NoError(t, err)
	assert.True(t, res.Installed)
	assert.Equal(t, "my-name", res.Name)
	// Installed under the override directory; the sub-skill's own dir name is not used.
	assert.FileExists(t, filepath.Join(userDirFn("u1"), "my-name", "SKILL.md"))
	assert.NoDirExists(t, filepath.Join(userDirFn("u1"), "gildata-finance-data"))
}

func TestInstallSkill_InstallDoc(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# Install guide\n\nTo install gildata-finance-data, clone https://example.com/collection "+
			"and select skill gildata-finance-data.\n")
	}))
	defer server.Close()

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.httpClient = server.Client()

	res, err := ins.installSkill(context.Background(), server.URL+"/skillhub.md", "", "", "u1")
	require.NoError(t, err)
	assert.Equal(t, "install-doc", res.Kind)
	assert.False(t, res.Installed)
	assert.Equal(t, server.URL+"/skillhub.md", res.URL)
	assert.Contains(t, res.Content, "gildata-finance-data")
	assert.NotEmpty(t, res.Hint)

	// Nothing is written to the user skills directory.
	entries, err := listSkills(loader, "u1")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestInstallSkill_SingleFile_NonTextErrors(t *testing.T) {
	loader := skill.NewLoader(t.TempDir(), func(string) string { return t.TempDir() })
	ins := newInstaller(loader, func(string) string { return t.TempDir() })

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer server.Close()
	ins.httpClient = server.Client()

	_, err := ins.installSkill(context.Background(), server.URL+"/icon.md", "", "", "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-text")
}

func TestInstallSkill_SingleFile_SkillArgIgnored(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "---\nname: from-url\ndescription: From URL\n---\n# Body")
	}))
	defer server.Close()

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.httpClient = server.Client()

	res, err := ins.installSkill(context.Background(), server.URL+"/skill.md", "", "some-sub-skill", "u1")
	require.NoError(t, err)
	assert.True(t, res.Installed)
	assert.Equal(t, "install", res.Kind)
	assert.Equal(t, "from-url", res.Name)
	assert.Contains(t, res.Note, "skill")
	assert.Contains(t, res.Note, "single-file")
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	collectionClone(t, root)
	// A directory whose SKILL.md lacks valid frontmatter must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "SKILL.md"), []byte("# just docs, no frontmatter"), 0o644))

	loader := skill.NewLoader(root, nil)
	got := loader.Discover(root)
	names := make([]string, 0, len(got))
	for _, s := range got {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"gildata-finance-data", "wind-data"}, names)
}

// TestInstallSkillTool_Execute_InstallDoc drives the real registered tool and
// confirms the Execute handler returns the discriminated install-doc result.
func TestInstallSkillTool_Execute_InstallDoc(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# Not a skill\n\nThis is an install guide.\n")
	}))
	defer server.Close()

	loader := skill.NewLoader(globalDir, userDirFn)
	tools := NewTools(loader, userDirFn).WithHTTPClient(server.Client())
	r := tool.NewRegistry()
	require.NoError(t, RegisterAll(r, tools))

	ctx := skill.WithUserID(context.Background(), "u1")
	out, err := r.Call(ctx, ToolInstallSkill, json.RawMessage(`{"url":"`+server.URL+`/skillhub.md"}`))
	require.NoError(t, err)

	res, ok := out.(installResult)
	require.True(t, ok)
	assert.Equal(t, "install-doc", res.Kind)
	assert.False(t, res.Installed)
	assert.Contains(t, res.Content, "install guide")
}

// TestInstallSkill_ManifestFlow simulates the agent-orchestrated install: the
// agent first points luban at an install doc, luban returns the doc content, the
// agent gleans the real source + sub-skill from it, and calls luban again to
// install the real source. (The LLM-in-the-loop reading step is the model's job;
// this test exercises the tool mechanics the loop relies on.)
func TestInstallSkill_ManifestFlow(t *testing.T) {
	globalDir := t.TempDir()
	dataDir := t.TempDir()
	userDirFn := func(userID string) string { return filepath.Join(dataDir, userID, "skills") }

	docServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# Install finance data\n\nReal source: https://example.com/collection\nSub-skill: gildata-finance-data\n")
	}))
	defer docServer.Close()

	loader := skill.NewLoader(globalDir, userDirFn)
	ins := newInstaller(loader, userDirFn)
	ins.httpClient = docServer.Client()
	ins.gitRunner = func(ctx context.Context, urlStr, targetDir string) error {
		collectionClone(t, targetDir)
		return nil
	}

	// Step 1: the agent points luban at the install doc URL.
	res, err := ins.installSkill(context.Background(), docServer.URL+"/skillhub.md", "", "", "u1")
	require.NoError(t, err)
	require.Equal(t, "install-doc", res.Kind)
	require.Contains(t, res.Content, "https://example.com/collection")
	require.Contains(t, res.Content, "gildata-finance-data")

	// Step 2: the agent reads the doc, gleans the real source + sub-skill, and re-installs.
	res, err = ins.installSkill(context.Background(), "https://example.com/collection", "", "gildata-finance-data", "u1")
	require.NoError(t, err)
	require.True(t, res.Installed)
	require.Equal(t, "gildata-finance-data", res.Name)
	assert.FileExists(t, filepath.Join(userDirFn("u1"), "gildata-finance-data", "SKILL.md"))
}

func writeSkill(t *testing.T, dir, name, description, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, description, body)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}
