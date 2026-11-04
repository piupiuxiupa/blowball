package config

import (
	"testing"
	"time"
)

func TestScheduledTasksConfigDefaultsOff(t *testing.T) {
	path := writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduledTasks.Enabled {
		t.Fatal("scheduled tasks must default to disabled")
	}
	if got := cfg.ScheduledTasks.PollIntervalValue(); got != 5*time.Second {
		t.Fatalf("poll interval = %s", got)
	}
	if got := cfg.ScheduledTasks.MaxConcurrentValue(); got != 4 {
		t.Fatalf("max concurrent = %d", got)
	}
	if got := cfg.ScheduledTasks.MaxRunDurationValue(); got != 10*time.Minute {
		t.Fatalf("max run duration = %s", got)
	}
}

func TestScheduledTasksConfigExplicitValuesAndValidation(t *testing.T) {
	valid := writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
scheduled_tasks:
  enabled: true
  poll_interval: 2s
  max_concurrent: 2
  max_run_duration: 3m
`)
	cfg, err := Load(valid)
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

	for _, block := range []string{
		"poll_interval: 0s",
		"poll_interval: -1s",
		"max_concurrent: 0",
		"max_concurrent: -1",
		"max_run_duration: 0s",
		"max_run_duration: -1s",
	} {
		t.Run(block, func(t *testing.T) {
			path := writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
scheduled_tasks:
  enabled: true
  `+block+"\n")
			if _, err := Load(path); err == nil {
				t.Fatalf("expected %s to fail config load", block)
			}
		})
	}
}
