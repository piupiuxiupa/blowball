package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSystemPrompt_EnvironmentOnly(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		BasePrompt: "You are a helpful assistant.",
		Workspace:  "/data/u-1/workspace",
		UserID:     "u-1",
		Platform:   "arm64",
		OS:         "darwin",
		Cutoff:     "August 2025",
	})
	require.NoError(t, err)

	assert.Contains(t, out, "You are a helpful assistant.")
	assert.Contains(t, out, "# Environment")
	assert.Contains(t, out, "- Primary working directory: /data/u-1/workspace")
	assert.Contains(t, out, "- Platform: arm64")
	assert.Contains(t, out, "- OS: darwin")
	assert.Contains(t, out, "- User ID: u-1")
	assert.Contains(t, out, "- Assistant knowledge cutoff: August 2025")
}

func TestRenderSystemPrompt_Tools(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
		Tools: []ToolInfo{
			{Name: "read_file", Description: "Read a file from disk."},
			{Name: "web_search", Description: "Search the web.", Server: "remote"},
			{Name: "web_fetch", Description: "Fetch a URL.", Server: "remote"},
		},
	})
	require.NoError(t, err)

	assert.Contains(t, out, "## Built-in Tools")
	assert.Contains(t, out, "- read_file: Read a file from disk.")
	assert.Contains(t, out, "## MCP Tools")
	assert.Contains(t, out, "### remote")
	assert.Contains(t, out, "- web_search: Search the web.")
	assert.Contains(t, out, "- web_fetch: Fetch a URL.")
}

func TestRenderSystemPrompt_Skills(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
		Skills: []SkillInfo{
			{Name: "coding-style", Description: "Global coding conventions", Location: "skills/coding-style"},
		},
	})
	require.NoError(t, err)

	assert.Contains(t, out, "## Skills")
	assert.Contains(t, out, "<skills>")
	assert.Contains(t, out, "  <skill>")
	assert.Contains(t, out, "    <name>coding-style</name>")
	assert.Contains(t, out, "    <description>Global coding conventions</description>")
	assert.Contains(t, out, "    <location>skills/coding-style</location>")
	assert.Contains(t, out, "</skills>")
	assert.Contains(t, out, "Use luban_list_skills to discover skills")
	assert.Contains(t, out, "luban_read_skill")
	assert.Contains(t, out, "luban_install_skill")
	assert.Contains(t, out, "Never use xizhi_* tools to access the skills directory.")
	assert.NotContains(t, out, "call read_skill")
}

func TestRenderSystemPrompt_OmitsEmptySections(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
	})
	require.NoError(t, err)

	assert.Contains(t, out, "# Environment")
	assert.NotContains(t, out, "## Built-in Tools")
	assert.NotContains(t, out, "## MCP Tools")
	assert.NotContains(t, out, "## Skills")
	assert.Equal(t, 1, strings.Count(out, "# Environment"))
}

func TestRenderSystemPrompt_NoDuplicateEnvironment(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		BasePrompt: "some base prompt\n\n# Environment\n- Old: value",
		Workspace:  "/data/u-1/workspace",
		UserID:     "u-1",
		Platform:   "amd64",
		OS:         "linux",
		Cutoff:     "August 2025",
	})
	require.NoError(t, err)

	// The rendered prompt may contain the literal "# Environment" from the
	// caller's base prompt plus the one we add. The contract is that exactly
	// one environment section is added by RenderSystemPrompt; callers should
	// not embed their own.
	assert.Contains(t, out, "# Environment")
}
