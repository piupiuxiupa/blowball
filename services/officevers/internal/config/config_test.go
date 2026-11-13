package config

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

// allFlags builds an args slice containing the four required values so that
// validation passes, letting each test focus on the behavior under test.
func requiredArgs() []string {
	return []string{
		"-minio-endpoint", "127.0.0.1:9000",
		"-minio-access-key", "ak",
		"-minio-secret-key", "sk",
		"-minio-bucket", "bucket",
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY",
		"MINIO_BUCKET", "MINIO_USE_SSL", "MINIO_REGION", "LISTEN_ADDR",
	} {
		t.Setenv(k, "")
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("MINIO_ENDPOINT", "from-env")
	t.Setenv("LISTEN_ADDR", ":9000")

	args := append(requiredArgs(), "-minio-endpoint", "from-flag")
	cfg, err := parse(args, io.Discard)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.MinIOEndpoint != "from-flag" {
		t.Fatalf("endpoint = %q, want from-flag (flag should override env)", cfg.MinIOEndpoint)
	}
	if cfg.ListenAddr != ":9000" {
		t.Fatalf("listen-addr = %q, want :9000 (env should apply when flag unset)", cfg.ListenAddr)
	}
}

func TestEnvOverridesDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("MINIO_ENDPOINT", "127.0.0.1:9000")
	t.Setenv("MINIO_ACCESS_KEY", "ak")
	t.Setenv("MINIO_SECRET_KEY", "sk")
	t.Setenv("MINIO_BUCKET", "from-env-bucket")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("MINIO_REGION", "us-east-1")

	cfg, err := Load([]string{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.MinIOBucket != "from-env-bucket" {
		t.Fatalf("bucket = %q, want from-env-bucket", cfg.MinIOBucket)
	}
	if cfg.ListenAddr != ":9999" {
		t.Fatalf("listen-addr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.MinIORegion != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1", cfg.MinIORegion)
	}
}

func TestDefaultApplied(t *testing.T) {
	clearEnv(t)
	cfg, err := parse(requiredArgs(), io.Discard)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("listen-addr = %q, want :8080 (built-in default)", cfg.ListenAddr)
	}
	if cfg.MinIOUseSSL {
		t.Fatal("use-ssl should default to false")
	}
}

func TestBoolFlag(t *testing.T) {
	clearEnv(t)
	args := append(requiredArgs(), "-minio-use-ssl")
	cfg, err := parse(args, io.Discard)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !cfg.MinIOUseSSL {
		t.Fatal("use-ssl should be true when flag is set")
	}

	// Explicit false overrides a true env value.
	t.Setenv("MINIO_USE_SSL", "true")
	if cfg2, err := parse(append(requiredArgs(), "-minio-use-ssl=false"), io.Discard); err != nil || cfg2.MinIOUseSSL {
		t.Fatalf("use-ssl=false flag should override true env; cfg=%v err=%v", cfg2.MinIOUseSSL, err)
	}
}

func TestDoubleDashWorks(t *testing.T) {
	clearEnv(t)
	// Go's flag package accepts both -flag and --flag.
	args := append([]string{
		"--minio-endpoint", "127.0.0.1:9000",
		"--minio-access-key", "ak",
		"--minio-secret-key", "sk",
		"--minio-bucket", "b",
	}, "--listen-addr", ":1234")
	cfg, err := parse(args, io.Discard)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.ListenAddr != ":1234" {
		t.Fatalf("listen-addr = %q, want :1234", cfg.ListenAddr)
	}
}

func TestMissingRequired(t *testing.T) {
	clearEnv(t)
	_, err := parse([]string{}, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing required config, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"minio-endpoint", "minio-access-key", "minio-secret-key", "minio-bucket"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing required name %q", msg, want)
		}
	}
}

func TestHelpFlagReturnsErrHelp(t *testing.T) {
	_, err := parse([]string{"-h"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

func TestUnknownFlagErrors(t *testing.T) {
	clearEnv(t)
	_, err := parse(append(requiredArgs(), "-no-such-flag"), io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	if !strings.Contains(err.Error(), "no-such-flag") && !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unexpected error: %v", err)
	}
}
