package config

import (
	"strings"
	"testing"
	"time"
)

func baseInternalRPCYAML(extra string) string {
	return `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
internal_rpc:
  enabled: true
  listen: ":9092"
  tls: {cert_file: /c/main.crt, key_file: /c/main.key, ca_file: /c/ca.crt}
  scheduler_endpoint: "127.0.0.1:9091"
  scheduler_tls: {cert_file: /c/client.crt, key_file: /c/client.key, ca_file: /c/ca.crt, server_name: blowball-scheduler}
` + extra
}

func TestInternalRPCConfigValid(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, baseInternalRPCYAML(`
scheduled_runtime:
  poll_interval: 2s
  lease_duration: 10s
  max_concurrent: 3
  max_prepare_attempts: 2
  scan_batch: 20
scheduled_resources:
  max_files: 10
  max_total_bytes: 1024
  max_file_bytes: 512
  max_context_files: 5
  max_context_bytes: 100
`)))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InternalRPC.Enabled {
		t.Fatal("internal_rpc should be enabled")
	}
	poll, lease, concurrent, attempts, scan := cfg.ScheduledRuntime.Values()
	if poll != 2*time.Second || lease != 10*time.Second || concurrent != 3 || attempts != 2 || scan != 20 {
		t.Fatalf("unexpected runtime values: %s %s %d %d %d", poll, lease, concurrent, attempts, scan)
	}
	if cfg.ScheduledResources.MaxFiles != 10 || cfg.ScheduledResources.MaxContextBytes != 100 {
		t.Fatalf("unexpected resource values: %+v", cfg.ScheduledResources)
	}
}

func TestInternalRPCConfigRequiresTLSAndEndpoints(t *testing.T) {
	path := writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
internal_rpc:
  enabled: true
  listen: ":9092"
  scheduler_endpoint: "127.0.0.1:9091"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "internal_rpc.tls") {
		t.Fatalf("expected a mutual-TLS requirement error, got %v", err)
	}

	path = writeTempYAML(t, strings.Replace(baseInternalRPCYAML(""), `scheduler_endpoint: "127.0.0.1:9091"`, "", 1))
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "scheduler_endpoint") {
		t.Fatalf("expected a scheduler endpoint requirement error, got %v", err)
	}
}

func TestScheduledRuntimeAndResourcesRejectNegativeBounds(t *testing.T) {
	path := writeTempYAML(t, baseInternalRPCYAML(`
scheduled_runtime:
  max_concurrent: -1
`))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("expected a positive max_concurrent error, got %v", err)
	}

	path = writeTempYAML(t, baseInternalRPCYAML(`
scheduled_resources:
  max_files: -1
`))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_files") {
		t.Fatalf("expected a positive max_files error, got %v", err)
	}
}

func TestInternalRPCConfigDisabledIsZeroBehaviorChange(t *testing.T) {
	cfg, err := Load(writeTempYAML(t, `
openai:
  api_key: sk-test
  models:
    - {name: gpt-test, max_context_tokens: 10000, max_completion_tokens: 1000}
mysql: {dsn: "user:pass@tcp(127.0.0.1:3306)/db"}
jwt: {secret: "ok"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InternalRPC.Enabled {
		t.Fatal("internal_rpc must be disabled by default")
	}
}
