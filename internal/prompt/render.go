// Package prompt renders system prompts from plain input data.
//
// It is intentionally decoupled from tool registries and skill loaders: callers
// are responsible for filtering and collecting the data they want rendered.
package prompt

import (
	"fmt"
	"sort"
	"strings"
)

// ToolInfo describes a tool to include in the system prompt.
type ToolInfo struct {
	Name        string
	Description string
	Server      string // Empty for built-in tools; non-empty for MCP server name.
}

// SkillInfo describes a skill to include in the system prompt catalog.
type SkillInfo struct {
	Name        string
	Description string
	Location    string
}

// MCPServerInfo describes a per-user MCP server to advertise in the system
// prompt. Only server-level name/description/url are rendered (never
// credentials); the agent discovers the per-server tool catalogue on demand
// via the mcp_* tools.
type MCPServerInfo struct {
	Name        string
	Description string
	URL         string
}

// RenderInput is the plain-data input to RenderSystemPrompt.
type RenderInput struct {
	BasePrompt      string
	Workspace       string
	GlobalSkillsDir string
	UserSkillsDir   string
	UserID          string
	Platform        string
	OS              string
	Cutoff          string

	Tools   []ToolInfo
	Skills  []SkillInfo
	UserMCP []MCPServerInfo
}

// RenderSystemPrompt renders a complete system prompt from the provided input.
// The output contains a single # Environment section, optionally followed by
// tool and skill sections. Empty sections are omitted.
func RenderSystemPrompt(input RenderInput) (string, error) {
	var b strings.Builder

	if input.BasePrompt != "" {
		b.WriteString(strings.TrimSpace(input.BasePrompt))
		b.WriteString("\n\n")
	}
	b.WriteString(renderImportantNotice())
	b.WriteString("\n\n")

	b.WriteString(renderWorkspaceConvention())
	b.WriteString("\n\n")

	b.WriteString(renderEnvironment(input))
	b.WriteString("\n\n")

	builtIn, mcpByServer := classifyTools(input.Tools)

	if len(builtIn) > 0 {
		b.WriteString("## Built-in Tools\n")
		for _, t := range builtIn {
			fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
		}
		b.WriteString("\n")
	}

	if len(mcpByServer) > 0 {
		b.WriteString("## MCP Tools\n")
		servers := make([]string, 0, len(mcpByServer))
		for name := range mcpByServer {
			servers = append(servers, name)
		}
		sort.Strings(servers)
		for _, serverName := range servers {
			fmt.Fprintf(&b, "### %s\n", serverName)
			for _, t := range mcpByServer[serverName] {
				fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Description)
			}
		}
		b.WriteString("\n")
	}

	if len(input.UserMCP) > 0 {
		b.WriteString("## User MCP Servers\n")
		b.WriteString("Per-user MCP servers configured in your workspace (`.blowball/mcp/`). " +
			"Use `mcp_list_servers` to inspect them. Before calling a server's tool with " +
			"`mcp_call(server, tool, args)`, you MUST first call `mcp_list_tools(server)` to " +
			"discover that server's exact tool names and input schemas; never guess a tool " +
			"name or construct an argument shape from memory, because a wrong guess is " +
			"rejected before the remote call is even made. Credentials are managed " +
			"server-side and are never shown to you.\n")
		for _, s := range input.UserMCP {
			if s.Description != "" {
				fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, s.Description, s.URL)
			} else {
				fmt.Fprintf(&b, "- %s (%s)\n", s.Name, s.URL)
			}
		}
		b.WriteString("\n")
		b.WriteString("The `.blowball/mcp/` namespace is managed exclusively via the `mcp_*` tools; " +
			"never use `xizhi_*` tools to read or modify `.blowball` or any MCP config.\n")
		b.WriteString("\n")
	}

	if len(input.Skills) > 0 {
		b.WriteString("## Skills\n")
		b.WriteString("Available skills:\n")
		b.WriteString("<skills>\n")
		for _, s := range input.Skills {
			fmt.Fprintf(&b, "  <skill>\n")
			fmt.Fprintf(&b, "    <name>%s</name>\n", s.Name)
			fmt.Fprintf(&b, "    <description>%s</description>\n", s.Description)
			fmt.Fprintf(&b, "    <location>%s</location>\n", s.Location)
			fmt.Fprintf(&b, "  </skill>\n")
		}
		b.WriteString("</skills>\n\n")
		b.WriteString("Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory.\n")
		b.WriteString("luban_install_skill supports several install shapes: a whole git repository is cloned as one entry; a git collection combined with the optional `skill` parameter installs only the selected sub-skill (matched by frontmatter name, else by repo-relative subpath) and discards the rest; and a single SKILL.md URL ending in .md is downloaded and installed directly.\n")
		b.WriteString("If a .md URL is not itself a valid skill, luban_install_skill returns the fetched content as an install document (result kind \"install-doc\") instead of installing. When a user asks to install a skill from an instruction or landing page, read the returned install-document content, follow it to the real skill source URL it points at, and call luban_install_skill again with that source - do not treat the instruction page itself as the skill.\n")
		b.WriteString("You may use the bash or python tools to read and execute files under the exposed skill directories. Global skill directories are read-only and must not be modified. Per-user skills live under the workspace at .blowball/skills and are managed exclusively via the luban_* tools; never use xizhi_* tools to access .blowball or any skill directory.\n")
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String()), nil
}

func renderEnvironment(input RenderInput) string {
	return fmt.Sprintf(`# Environment
- Global skills directory: %s
- User skills directory: %s
- Platform: %s
- OS: %s
- User ID: %s
- Assistant knowledge cutoff: %s`, input.GlobalSkillsDir, input.UserSkillsDir, input.Platform, input.OS, input.UserID, input.Cutoff)
}

func renderWorkspaceConvention() string {
	return "## Workspace path convention\n" +
		"- All `xizhi_*` paths must be relative to the workspace root. Use paths like `tmp/hello.txt` or `src/main.go`, not `/workspace/...` or absolute paths.\n" +
		"- The `bash` and `python` sandboxes run with `/workspace` as the working directory.\n" +
		"- The sandbox's `/tmp` is mapped to the workspace's `./tmp/` directory. Files written to `/tmp` persist at `tmp/` and can be read with `xizhi_read_file` using a relative path such as `tmp/hello.txt`."
}

func classifyTools(tools []ToolInfo) ([]ToolInfo, map[string][]ToolInfo) {
	var builtIn []ToolInfo
	mcpByServer := make(map[string][]ToolInfo)
	for _, t := range tools {
		if t.Server == "" {
			builtIn = append(builtIn, t)
			continue
		}
		mcpByServer[t.Server] = append(mcpByServer[t.Server], t)
	}
	return builtIn, mcpByServer
}

func renderImportantNotice() string {
	return `
	**NOTICE**:
	- Before processing a task, you will first check whether the most suitable skill is available for use, and only then look for other callable tools. 
	- Before executing a task, also proactively evaluate whether a configured per-user MCP service (see the "User MCP Servers" section, if present) is the best way to accomplish it, and select it when appropriate.
	- You must not repeat the content of the skill itself to the user; instead, you should strictly follow the specifications of the skill to complete the task.
	- When replying to users, try to use few or no emojis.
	`
}
