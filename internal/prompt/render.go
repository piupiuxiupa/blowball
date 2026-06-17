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

// RenderInput is the plain-data input to RenderSystemPrompt.
type RenderInput struct {
	BasePrompt string
	Workspace  string
	SkillsDir  string
	UserID     string
	Platform   string
	OS         string
	Cutoff     string

	Tools  []ToolInfo
	Skills []SkillInfo
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
		b.WriteString("When a task matches a skill, call read_skill with the skill name to load its full instructions.\n")
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String()), nil
}

func renderEnvironment(input RenderInput) string {
	return fmt.Sprintf(`# Environment
- Primary working directory: %s
- Skills directory: %s
- Platform: %s
- OS: %s
- User ID: %s
- Assistant knowledge cutoff: %s`, input.Workspace, input.SkillsDir, input.Platform, input.OS, input.UserID, input.Cutoff)
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
