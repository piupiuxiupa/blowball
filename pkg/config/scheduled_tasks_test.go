package config

import (
	"strings"
	"testing"
	"time"
)

func baseScheduledConfig(extra string) string {
	return `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
scheduled_tasks:
` + extra
}

func TestLegacyEnabledSchedulerConfigIsRejected(t *testing.T) {
	path := writeTempYAML(t, baseScheduledConfig("  enabled: true\n"))
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected the removed embedded scheduler to be rejected")
	}
	if !strings.Contains(err.Error(), "standalone Scheduler") {
		t.Fatalf("error should point at the migration, got %v", err)
	}
}

func TestLegacyScheduledTasksBlockRemainsParseableWhenDisabled(t *testing.T) {
	path := writeTempYAML(t, baseScheduledConfig("  enabled: false\n  poll_interval: 2s\n  max_concurrent: 2\n  max_run_duration: 3m\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ScheduledTasks.PollIntervalValue(); got != 2*time.Second {
		t.Fatalf("poll interval = %s", got)
	}
	if got := cfg.ScheduledTasks.MaxConcurrentValue(); got != 2 {
		t.Fatalf("max concurrent = %d", got)
	}
	if got := cfg.ScheduledTasks.MaxRunDurationValue(); got != 3*time.Minute {
		t.Fatalf("max run duration = %s", got)
	}
}
