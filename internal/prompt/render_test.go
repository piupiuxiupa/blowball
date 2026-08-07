package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSystemPrompt_EnvironmentOnly(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		BasePrompt:      "You are a helpful assistant.",
		Workspace:       "/data/u-1/workspace",
		GlobalSkillsDir: "/skills/global",
		UserSkillsDir:   "/workspace/.blowball/skills",
		UserID:          "u-1",
		Platform:        "arm64",
		OS:              "darwin",
		Cutoff:          "August 2025",
	})
	require.NoError(t, err)

	assert.Contains(t, out, "You are a helpful assistant.")
	assert.Contains(t, out, "# Environment")
	assert.Contains(t, out, "- Global skills directory: /skills/global")
	assert.Contains(t, out, "- User skills directory: /workspace/.blowball/skills")
	assert.Contains(t, out, "- Platform: arm64")
	assert.Contains(t, out, "- OS: darwin")
	assert.Contains(t, out, "- User ID: u-1")
	assert.Contains(t, out, "- Assistant knowledge cutoff: August 2025")
	assert.NotContains(t, out, "- Workspace root: /data/u-1/workspace")
}

func TestRenderSystemPrompt_WorkspacePathConvention(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		BasePrompt: "You are a helpful assistant.",
		Workspace:  "/data/u-1/workspace",
		UserID:     "u-1",
		Platform:   "amd64",
		OS:         "linux",
		Cutoff:     "August 2025",
	})
	require.NoError(t, err)

	assert.Contains(t, out, "## Workspace path convention")
	assert.Contains(t, out, "All `xizhi_*` paths must be relative to the workspace root")
	assert.Contains(t, out, "`tmp/hello.txt`")
	assert.Contains(t, out, "`/workspace`")
	assert.Contains(t, out, "`xizhi_read_file`")
	assert.NotContains(t, out, "/data/u-1/workspace")
}

// TestRenderSystemPrompt_FileOutputConvention pins the workspace output +
// tmp-cleanup guidance added to renderWorkspaceConvention (delta spec
// "Workspace file output convention and tmp cleanup guidance").
func TestRenderSystemPrompt_FileOutputConvention(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
	})
	require.NoError(t, err)

	// (a) temporary vs deliverables split.
	assert.Contains(t, out, "Where generated files go")
	assert.Contains(t, out, "tmp/")
	// (b) grouping related files.
	assert.Contains(t, out, "keep related files together")
	// (c) timely tmp cleanup via xizhi_delete.
	assert.Contains(t, out, "Keep `tmp/` clean")
	assert.Contains(t, out, "xizhi_delete")
	// (d) forbid handing tmp paths as deliverables.
	assert.Contains(t, out, "Never hand a `tmp/` path to the user as a deliverable")
	// Coexists with the pre-existing relative-path / tmp-mapping guidance.
	assert.Contains(t, out, "All `xizhi_*` paths must be relative to the workspace root")
	assert.Contains(t, out, "sandbox's `/tmp` is mapped")
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
	assert.Contains(t, out, "Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory.")
	assert.Contains(t, out, "luban_read_skill")
	assert.Contains(t, out, "luban_install_skill")
	// Multi-form install guidance: supported shapes and install-doc flow.
	assert.Contains(t, out, "whole git repository is cloned as one entry")
	assert.Contains(t, out, "selected sub-skill")
	assert.Contains(t, out, "single SKILL.md URL ending in .md")
	assert.Contains(t, out, "install document")
	assert.Contains(t, out, "install-doc")
	assert.Contains(t, out, "follow it to the real skill source URL it points at")
	assert.Contains(t, out, "do not treat the instruction page itself as the skill")
	assert.Contains(t, out, "You may use the bash or python tools to read and execute files under the exposed skill directories.")
	assert.Contains(t, out, "Global skill directories are read-only")
	assert.Contains(t, out, "Per-user skills live under the workspace at .blowball/skills")
	assert.Contains(t, out, "managed exclusively via the luban_* tools")
	assert.Contains(t, out, "never use xizhi_* tools to access .blowball or any skill directory")
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

func TestRenderSystemPrompt_UserMCPServers(t *testing.T) {
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
		UserMCP: []MCPServerInfo{
			{Name: "github", Description: "GitHub issues & PRs", URL: "https://mcp/mcp"},
			{Name: "linear", URL: "https://linear/mcp"},
		},
	})
	require.NoError(t, err)

	// 6.1: per-user servers rendered in their own section, distinct from
	// operator MCP.
	assert.Contains(t, out, "## User MCP Servers")
	assert.Contains(t, out, "github")
	assert.Contains(t, out, "GitHub issues & PRs")
	assert.Contains(t, out, "linear")
	assert.Contains(t, out, "mcp_call")
	assert.Contains(t, out, "mcp_list_servers")
}

func TestRenderSystemPrompt_UserMCPListBeforeCallConvention(t *testing.T) {
	// task 6.4: the User MCP Servers section states the list-before-call
	// convention — call mcp_list_tools before mcp_call, never guess.
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
		UserMCP:   []MCPServerInfo{{Name: "github", URL: "https://mcp/mcp"}},
	})
	require.NoError(t, err)
	assert.Contains(t, out, "## User MCP Servers")
	assert.Contains(t, out, "mcp_list_tools", "convention must name the discovery tool")
	assert.Contains(t, out, "mcp_call")
	assert.Contains(t, out, "never guess", "convention must forbid guessing tool names/args")
	assert.Contains(t, out, "rejected before the remote call", "convention must explain the rejection")
}

func TestRenderSystemPrompt_ProactiveSelectionNudge(t *testing.T) {
	// 6.2: the important notice nudges the agent to proactively evaluate and
	// select a per-user MCP service when appropriate.
	out, err := RenderSystemPrompt(RenderInput{
		Workspace: "/data/u-1/workspace",
		UserID:    "u-1",
		Platform:  "amd64",
		OS:        "linux",
		Cutoff:    "August 2025",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "proactively evaluate")
	assert.Contains(t, out, "User MCP Servers")
}

func TestRenderSystemPrompt_BlowballMCPConstraint(t *testing.T) {
	// 6.3: the .blowball/mcp management constraint is stated only when the
	// per-user MCP section is present.
	without := mustRender(t, RenderInput{
		Workspace: "/data/u-1/workspace", UserID: "u-1", Platform: "amd64", OS: "linux", Cutoff: "August 2025",
	})
	assert.NotContains(t, without, ".blowball/mcp/")

	with := mustRender(t, RenderInput{
		Workspace: "/data/u-1/workspace", UserID: "u-1", Platform: "amd64", OS: "linux", Cutoff: "August 2025",
		UserMCP: []MCPServerInfo{{Name: "github", URL: "https://mcp/mcp"}},
	})
	assert.Contains(t, with, "`.blowball/mcp/` namespace is managed exclusively via the `mcp_*` tools")
	assert.Contains(t, with, "never use `xizhi_*` tools")
}

func mustRender(t *testing.T, in RenderInput) string {
	t.Helper()
	out, err := RenderSystemPrompt(in)
	require.NoError(t, err)
	return out
}
