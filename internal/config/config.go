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

// AgentConfig describes a single agent's runtime settings.
type AgentConfig struct {
	Name         string   `yaml:"name"`
	Model        string   `yaml:"model"`
	SystemPrompt string   `yaml:"system_prompt"`
	MaxTokens    int      `yaml:"max_tokens"`
	Tools        []string `yaml:"tools"`
}

// AgentsConfig holds the three blowball agents.
type AgentsConfig struct {
	Confuse  AgentConfig `yaml:"confuse"`
	Chongzhi AgentConfig `yaml:"chongzhi"`
	Liang    AgentConfig `yaml:"liang"`
}

// XizhiToolConfig is the enabled flag for a single Xizhi tool.
type XizhiToolConfig struct {
	Enabled bool `yaml:"enabled"`
}

// XizhiConfig groups the three Xizhi file tools.
type XizhiConfig struct {
	Read   XizhiToolConfig `yaml:"read"`
	Write  XizhiToolConfig `yaml:"write"`
	Modify XizhiToolConfig `yaml:"modify"`
}

// ToolsConfig groups all tool configuration.
type ToolsConfig struct {
	Xizhi XizhiConfig `yaml:"xizhi"`
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
	return nil
}

// expandEnv replaces ${VAR} and ${VAR:default} references in s with the
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
