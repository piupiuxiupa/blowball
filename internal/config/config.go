// Package config loads and validates the blowball backend configuration.
//
// Configuration is read from a YAML file. Values may reference environment
// variables using the ${VAR} or ${VAR:default} syntax; the loader expands
// them via os.ExpandEnv before unmarshalling.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration tree mirroring config.yaml.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	OpenAI  OpenAIConfig  `yaml:"openai"`
	MySQL   MySQLConfig   `yaml:"mysql"`
	Redis   RedisConfig   `yaml:"redis"`
	JWT     JWTConfig     `yaml:"jwt"`
	Agents  AgentsConfig  `yaml:"agents"`
	Tools   ToolsConfig   `yaml:"tools"`
	MCP     MCPConfig     `yaml:"mcp"`
	Logging LoggingConfig `yaml:"logging"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `yaml:"port"`
}

// OpenAIConfig holds OpenAI API client settings.
type OpenAIConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// MySQLConfig holds MySQL connection settings.
type MySQLConfig struct {
	DSN      string `yaml:"dsn"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// JWTConfig holds JWT signing settings.
type JWTConfig struct {
	Secret string `yaml:"secret"`
	Expire string `yaml:"expire"`
}

// ParseDuration resolves the configured expire duration. The value may be a
// plain Go time.Duration string (e.g. "24h", "30m") or a short form with a
// trailing unit suffix d/w/h/m (e.g. "7d", "2w", "30m"). Unsupported values
// fall back to time.ParseDuration.
func (j JWTConfig) ParseDuration() (time.Duration, error) {
	raw := strings.TrimSpace(j.Expire)
	if raw == "" {
		return 0, fmt.Errorf("jwt.expire is empty")
	}
	return parseDuration(raw)
}

// AgentMCPConfig holds per-agent MCP server and tool allowlists.
type AgentMCPConfig struct {
	Servers []AgentMCPServerConfig `yaml:"servers"`
}

// AgentMCPServerConfig declares one allowed MCP server and the tools from it
// the agent may use. Tools ["*"] allows every tool discovered from that server.
type AgentMCPServerConfig struct {
	Name  string   `yaml:"name"`
	Tools []string `yaml:"tools"`
}

// AgentConfig describes a single agent's runtime settings.
type AgentConfig struct {
	Name           string         `yaml:"name"`
	Model          string         `yaml:"model"`
	SystemPrompt   string         `yaml:"system_prompt"`
	MaxTokens      int            `yaml:"max_tokens"`
	Tools          []string       `yaml:"tools"`
	MCP            AgentMCPConfig `yaml:"mcp"`
	Skills         []string       `yaml:"skills"`
	Thinking       bool           `yaml:"thinking"`
	ReasoningEffort string        `yaml:"reasoning_effort"`
}

// AgentsConfig holds the three blowball agents.
type AgentsConfig struct {
	Confuse  AgentConfig `yaml:"confuse"`
	Chongzhi AgentConfig `yaml:"chongzhi"`
	Liang    AgentConfig `yaml:"liang"`
}

// validate checks every agent's MCP server references point to a declared
// global MCP server and that reasoning_effort values are valid. Tool and skill
// existence are validated later once the remote tool list and skill directories
// are known.
func (a *AgentsConfig) validate(serverNames map[string]struct{}) error {
	for _, name := range []string{"confuse", "chongzhi", "liang"} {
		var cfg *AgentConfig
		switch name {
		case "confuse":
			cfg = &a.Confuse
		case "chongzhi":
			cfg = &a.Chongzhi
		case "liang":
			cfg = &a.Liang
		}
		if cfg.Thinking {
			if cfg.ReasoningEffort == "" {
				cfg.ReasoningEffort = "medium"
			}
			if cfg.ReasoningEffort != "low" && cfg.ReasoningEffort != "medium" && cfg.ReasoningEffort != "high" {
				return fmt.Errorf("agents.%s.reasoning_effort: invalid value %q (must be low, medium, or high)", name, cfg.ReasoningEffort)
			}
		} else if cfg.ReasoningEffort != "" {
			return fmt.Errorf("agents.%s.reasoning_effort: cannot be set when thinking is disabled", name)
		}
		for i, s := range cfg.MCP.Servers {
			if strings.TrimSpace(s.Name) == "" {
				return fmt.Errorf("agents.%s.mcp.servers[%d]: name must be non-empty", name, i)
			}
			if _, ok := serverNames[s.Name]; !ok {
				return fmt.Errorf("agents.%s.mcp.servers[%d]: unknown mcp server %q", name, i, s.Name)
			}
		}
	}
	return nil
}

// XizhiToolConfig is the enabled flag for a single Xizhi tool.
type XizhiToolConfig struct {
	Enabled bool `yaml:"enabled"`
}

// XizhiConfig groups the Xizhi workspace-scoped file tools.
type XizhiConfig struct {
	Read      XizhiToolConfig `yaml:"read"`
	Write     XizhiToolConfig `yaml:"write"`
	Modify    XizhiToolConfig `yaml:"modify"`
	ListFiles XizhiToolConfig `yaml:"list_files"`
	Tree      XizhiToolConfig `yaml:"tree"`
	GlobFiles XizhiToolConfig `yaml:"glob_files"`
}

// WebfetchConfig holds the process-level webfetch tool settings.
type WebfetchConfig struct {
	Enabled bool          `yaml:"enabled"`
	Timeout time.Duration `yaml:"timeout"`
}

// ToolsConfig groups all tool configuration.
type ToolsConfig struct {
	Xizhi    XizhiConfig    `yaml:"xizhi"`
	Webfetch WebfetchConfig `yaml:"webfetch"`
}

// MCPConfig holds external MCP server configuration.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig describes a single external MCP server.
type MCPServerConfig struct {
	Name           string            `yaml:"name"`
	Transport      string            `yaml:"transport"`
	URL            string            `yaml:"url"`
	Command        string            `yaml:"command"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	Headers        map[string]string `yaml:"headers"`
	Timeout        time.Duration     `yaml:"timeout"`
	CallTimeout    time.Duration     `yaml:"call_timeout"`
	Reconnect      bool              `yaml:"reconnect"`
	Prefix         string            `yaml:"prefix"`
}

// LoggingConfig holds structured logging settings.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load reads the YAML config at path, expands ${VAR} / ${VAR:default}
// environment references, unmarshals it into a Config, and validates required
// fields. It returns an error if the file cannot be read, parsed, or fails
// validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	expanded := expandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate enforces required fields after loading.
func (c *Config) validate() error {
	if strings.TrimSpace(c.JWT.Secret) == "" {
		return fmt.Errorf("config validation error: jwt.secret must be non-empty")
	}
	if strings.TrimSpace(c.MySQL.DSN) == "" {
		return fmt.Errorf("config validation error: mysql.dsn must be non-empty")
	}
	if err := c.MCP.validate(); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	if err := c.Agents.validate(c.MCP.serverNames()); err != nil {
		return fmt.Errorf("config validation error: %w", err)
	}
	return nil
}

// validate checks every configured MCP server for required fields and
// uniqueness.
func (m MCPConfig) validate() error {
	seen := make(map[string]struct{}, len(m.Servers))
	for i, s := range m.Servers {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("mcp.servers[%d]: name must be non-empty", i)
		}
		if strings.TrimSpace(s.Transport) == "" {
			return fmt.Errorf("mcp.servers[%d]: transport must be non-empty", i)
		}
		switch s.Transport {
		case "sse":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): url is required for sse transport", i, s.Name)
			}
		case "http":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): url is required for http transport", i, s.Name)
			}
		case "stdio":
			if strings.TrimSpace(s.Command) == "" {
				return fmt.Errorf("mcp.servers[%d] (name=%q): command is required for stdio transport", i, s.Name)
			}
		default:
			return fmt.Errorf("mcp.servers[%d] (name=%q): unsupported transport %q", i, s.Name, s.Transport)
		}
		if _, exists := seen[s.Name]; exists {
			return fmt.Errorf("mcp.servers: duplicate server name %q", s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

// serverNames returns the set of declared global MCP server names.
func (m MCPConfig) serverNames() map[string]struct{} {
	out := make(map[string]struct{}, len(m.Servers))
	for _, s := range m.Servers {
		out[s.Name] = struct{}{}
	}
	return out
}

// ValidateAgentMCPTools checks every concrete tool name listed in agent MCP
// configurations against the discovered tools for the referenced server.
// serverTools maps server name to the set of prefixed tool names discovered from
// that server. A wildcard ("*") entry is always valid. The function is intended
// to be called after MCP client registration has populated serverTools.
func (c *Config) ValidateAgentMCPTools(serverTools map[string]map[string]struct{}) error {
	for _, agentName := range []string{"confuse", "chongzhi", "liang"} {
		var cfg AgentConfig
		switch agentName {
		case "confuse":
			cfg = c.Agents.Confuse
		case "chongzhi":
			cfg = c.Agents.Chongzhi
		case "liang":
			cfg = c.Agents.Liang
		}
		for _, s := range cfg.MCP.Servers {
			known, ok := serverTools[s.Name]
			if !ok {
				return fmt.Errorf("agents.%s.mcp.servers: unknown server %q", agentName, s.Name)
			}
			for _, toolName := range s.Tools {
				if toolName == "*" {
					continue
				}
				if _, exists := known[toolName]; !exists {
					return fmt.Errorf("agents.%s.mcp.servers[%q]: unknown tool %q", agentName, s.Name, toolName)
				}
			}
		}
	}
	return nil
}

// ValidateAgentSkills checks every skill name listed in agent configurations
// against the union of global and per-user skill names for the supplied userID.
// The hasSkill function should report whether a skill with the given name
// exists. An empty userID checks only the global skill directory.
func (c *Config) ValidateAgentSkills(userID string, hasSkill func(name, userID string) bool) error {
	for _, agentName := range []string{"confuse", "chongzhi", "liang"} {
		var cfg AgentConfig
		switch agentName {
		case "confuse":
			cfg = c.Agents.Confuse
		case "chongzhi":
			cfg = c.Agents.Chongzhi
		case "liang":
			cfg = c.Agents.Liang
		}
		for _, skillName := range cfg.Skills {
			if !hasSkill(skillName, userID) {
				return fmt.Errorf("agents.%s.skills: unknown skill %q", agentName, skillName)
			}
		}
	}
	return nil
}
// corresponding environment variable values. A reference with a default is
// left as the default when the variable is unset; a reference without a
// default becomes empty when unset, matching os.ExpandEnv semantics while
// adding optional defaults.
func expandEnv(s string) string {
	return envExpander.ReplaceAllStringFunc(s, func(match string) string {
		// Strip surrounding ${ and }.
		inner := match[2 : len(match)-1]
		name := inner
		def := ""
		if i := strings.IndexByte(inner, ':'); i >= 0 {
			name = inner[:i]
			def = inner[i+1:]
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	})
}

var envExpander = regexpMustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:[^}]*)?\}`)
