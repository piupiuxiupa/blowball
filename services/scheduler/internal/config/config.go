// Package config loads the standalone Scheduler's configuration. It expands
// ${VAR} / ${VAR:default} from the process environment, validates every
// positive bound at load time, and deliberately has no LLM, workspace,
// Landlock or Agent section.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	mainconfig "github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/rpcx"
)

// Config is the Scheduler process configuration.
type Config struct {
	Logging     mainconfig.LoggingConfig `yaml:"logging"`
	MySQL       MySQLConfig              `yaml:"mysql"`
	RPC         RPCConfig                `yaml:"rpc"`
	MainService MainServiceConfig        `yaml:"main_service"`
	Scheduler   SchedulerConfig          `yaml:"scheduler"`
}

// MySQLConfig points at the Scheduler-owned tables.
type MySQLConfig struct {
	DSN string `yaml:"dsn"`
}

// RPCConfig is the Scheduler's own mTLS listener.
type RPCConfig struct {
	SecurityMode rpcx.SecurityMode        `yaml:"security_mode"`
	PeerTokens   mainconfig.RPCPeerTokens `yaml:"peer_tokens"`
	Listen       string                   `yaml:"listen"`
	TLS          rpcx.TLSFiles            `yaml:"tls"`
	Timeout      time.Duration            `yaml:"timeout"`
}

// MainServiceConfig contains the endpoints the Scheduler calls: the API
// control surface for validation/resources and the Agent runtime surface for
// execution. Both may point at the same combined-role listener.
type MainServiceConfig struct {
	ClientToken   string        `yaml:"client_token"`
	APIEndpoint   string        `yaml:"api_endpoint"`
	AgentEndpoint string        `yaml:"agent_endpoint"`
	TLS           rpcx.TLSFiles `yaml:"tls"`
	Timeout       time.Duration `yaml:"timeout"`
}

// SchedulerConfig bounds materialization, submission and execution tracking.
// Pointer fields distinguish "omitted" (default) from an explicit non-positive
// value (rejected).
type SchedulerConfig struct {
	ExecutionEnabled *bool          `yaml:"execution_enabled"`
	PollInterval     *time.Duration `yaml:"poll_interval"`
	MaxConcurrent    *int           `yaml:"max_concurrent"`
	MaxRunDuration   *time.Duration `yaml:"max_run_duration"`
	LeaseDuration    *time.Duration `yaml:"lease_duration"`
	MaxAttempts      *int           `yaml:"max_attempts"`
	RetryBackoff     *time.Duration `yaml:"retry_backoff"`
	ScanBatch        *int           `yaml:"scan_batch"`
	MaxInputTokens   *int           `yaml:"max_input_tokens"`
}

// Values returns the effective scheduling values with defaults applied.
func (s SchedulerConfig) Values() (enabled bool, poll, maxRun, lease, backoff time.Duration, concurrent, attempts, scanBatch, maxInputTokens int) {
	enabled = s.ExecutionEnabled != nil && *s.ExecutionEnabled
	pick := func(v *time.Duration, def time.Duration) time.Duration {
		if v != nil {
			return *v
		}
		return def
	}
	pickInt := func(v *int, def int) int {
		if v != nil {
			return *v
		}
		return def
	}
	return enabled,
		pick(s.PollInterval, 5*time.Second),
		pick(s.MaxRunDuration, 10*time.Minute),
		pick(s.LeaseDuration, 30*time.Second),
		pick(s.RetryBackoff, time.Second),
		pickInt(s.MaxConcurrent, 4),
		pickInt(s.MaxAttempts, 3),
		pickInt(s.ScanBatch, 100),
		pickInt(s.MaxInputTokens, 5000)
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([^}]*))?\}`)

// Load reads and validates the Scheduler config.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scheduler config: read %s: %w", path, err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("scheduler config: parse %s: %w", path, err)
	}
	expanded := expandTree(tree)
	data, err := yaml.Marshal(expanded)
	if err != nil {
		return nil, fmt.Errorf("scheduler config: re-encode: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("scheduler config: decode: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.RPC.SecurityMode == "" {
		c.RPC.SecurityMode = rpcx.SecurityModeMTLS
	}
	if c.RPC.Listen == "" {
		c.RPC.Listen = ":9091"
	}
	if c.RPC.Timeout <= 0 {
		c.RPC.Timeout = rpcx.DefaultTimeout
	}
	if c.MainService.Timeout <= 0 {
		c.MainService.Timeout = rpcx.DefaultTimeout
	}
	// The Scheduler dials the main service as a CLIENT; the main server's
	// certificate identity is blowball-main when unset.
	if c.MainService.TLS.ServerName == "" {
		c.MainService.TLS.ServerName = "blowball-main"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}
	if len(c.Logging.Output) == 0 {
		c.Logging.Output = []string{"stdout"}
	}
}

func (c *Config) validate() error {
	if c.MySQL.DSN == "" {
		return fmt.Errorf("scheduler config: mysql.dsn is required")
	}
	if c.RPC.Listen == "" {
		return fmt.Errorf("scheduler config: rpc.listen is required")
	}
	mode, err := rpcx.NormalizeSecurityMode(c.RPC.SecurityMode)
	if err != nil {
		return fmt.Errorf("scheduler config: rpc.security_mode: %w", err)
	}
	if mode == rpcx.SecurityModeMTLS {
		if err := requireTLS("rpc.tls", c.RPC.TLS); err != nil {
			return err
		}
	} else {
		if err := rpcx.ValidatePeerTokens(c.RPC.PeerTokens); err != nil {
			return fmt.Errorf("scheduler config: rpc.peer_tokens: %w", err)
		}
		if err := rpcx.ValidateClientToken(c.MainService.ClientToken); err != nil {
			return fmt.Errorf("scheduler config: main_service.client_token: %w", err)
		}
	}
	if c.MainService.APIEndpoint == "" {
		return fmt.Errorf("scheduler config: main_service.api_endpoint is required")
	}
	if mode == rpcx.SecurityModeMTLS {
		if err := requireTLS("main_service.tls", c.MainService.TLS); err != nil {
			return err
		}
	}
	enabled, poll, maxRun, lease, backoff, concurrent, attempts, scan, maxInput := c.Scheduler.Values()
	if enabled && c.MainService.AgentEndpoint == "" {
		return fmt.Errorf("scheduler config: main_service.agent_endpoint is required when execution is enabled")
	}
	for name, v := range map[string]time.Duration{
		"scheduler.poll_interval": poll, "scheduler.max_run_duration": maxRun,
		"scheduler.lease_duration": lease, "scheduler.retry_backoff": backoff,
	} {
		if v <= 0 {
			return fmt.Errorf("scheduler config: %s must be positive", name)
		}
	}
	for name, v := range map[string]int{
		"scheduler.max_concurrent": concurrent, "scheduler.max_attempts": attempts,
		"scheduler.scan_batch": scan, "scheduler.max_input_tokens": maxInput,
	} {
		if v <= 0 {
			return fmt.Errorf("scheduler config: %s must be positive", name)
		}
	}
	return nil
}

func requireTLS(name string, files rpcx.TLSFiles) error {
	if files.CertFile == "" || files.KeyFile == "" || files.CAFile == "" {
		return fmt.Errorf("scheduler config: %s cert_file, key_file and ca_file are required (mutual TLS)", name)
	}
	return nil
}

func expandTree(value any) any {
	switch v := value.(type) {
	case string:
		return expandString(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = expandTree(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = expandTree(item)
		}
		return out
	default:
		return value
	}
}

func expandString(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		parts := envPattern.FindStringSubmatch(match)
		if value, ok := os.LookupEnv(parts[1]); ok && value != "" {
			return value
		}
		return parts[2]
	})
}
