// Package config loads runtime configuration for the office-version-service.
//
// Configuration sources, in order of precedence (highest first):
//  1. Command-line flags
//  2. Environment variables
//  3. Built-in defaults
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for the service.
type Config struct {
	// MinIOEndpoint is the host:port of the MinIO (or S3-compatible) endpoint.
	MinIOEndpoint string
	// MinIOAccessKey / MinIOSecretKey are the credentials used to access MinIO.
	MinIOAccessKey string
	MinIOSecretKey string
	// MinIOBucket is the single versioned bucket that stores all files.
	MinIOBucket string
	// MinIOUseSSL toggles TLS for the MinIO connection.
	MinIOUseSSL bool
	// MinIORegion is an optional region for the MinIO endpoint.
	MinIORegion string

	// ListenAddr is the HTTP address the service listens on (e.g. ":8080").
	ListenAddr string
}

// Load parses configuration with the precedence: command-line flags >
// environment variables > built-in defaults. Pass nil for args to use
// os.Args[1:]. It returns an error listing every missing required value.
func Load(args []string) (Config, error) {
	if args == nil {
		args = os.Args[1:]
	}
	return parse(args, os.Stderr)
}

func parse(args []string, errOut io.Writer) (Config, error) {
	fs := flag.NewFlagSet("office-version-service", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(errOut, "Usage of office-version-service:\n\n")
		fmt.Fprintf(errOut, "  Configuration precedence: flags > environment variables > defaults.\n\n")
		fs.PrintDefaults()
		fmt.Fprintf(errOut, "\nEnvironment variables:\n")
		fmt.Fprintf(errOut, "  MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY, MINIO_BUCKET,\n")
		fmt.Fprintf(errOut, "  MINIO_USE_SSL, MINIO_REGION, LISTEN_ADDR\n")
	}

	// Each flag's default is the env-var value (or the built-in default), so a
	// flag passed on the command line overrides the environment, while an unset
	// flag transparently falls back to the environment.
	endpoint := fs.String("minio-endpoint", envStr("MINIO_ENDPOINT", ""),
		"MinIO host:port, e.g. 127.0.0.1:9000 (env: MINIO_ENDPOINT)")
	accessKey := fs.String("minio-access-key", envStr("MINIO_ACCESS_KEY", ""),
		"MinIO access key (env: MINIO_ACCESS_KEY)")
	secretKey := fs.String("minio-secret-key", envStr("MINIO_SECRET_KEY", ""),
		"MinIO secret key (env: MINIO_SECRET_KEY)")
	bucket := fs.String("minio-bucket", envStr("MINIO_BUCKET", ""),
		"bucket for versioned files, created if missing (env: MINIO_BUCKET)")
	useSSL := fs.Bool("minio-use-ssl", envBool("MINIO_USE_SSL", false),
		"use TLS for the MinIO connection (env: MINIO_USE_SSL)")
	region := fs.String("minio-region", envStr("MINIO_REGION", ""),
		"optional MinIO region (env: MINIO_REGION)")
	listenAddr := fs.String("listen-addr", envStr("LISTEN_ADDR", ":8080"),
		"HTTP listen address, e.g. :8080 (env: LISTEN_ADDR)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg := Config{
		MinIOEndpoint:  *endpoint,
		MinIOAccessKey: *accessKey,
		MinIOSecretKey: *secretKey,
		MinIOBucket:    *bucket,
		MinIOUseSSL:    *useSSL,
		MinIORegion:    *region,
		ListenAddr:     *listenAddr,
	}

	var missing []string
	if cfg.MinIOEndpoint == "" {
		missing = append(missing, "minio-endpoint / MINIO_ENDPOINT")
	}
	if cfg.MinIOAccessKey == "" {
		missing = append(missing, "minio-access-key / MINIO_ACCESS_KEY")
	}
	if cfg.MinIOSecretKey == "" {
		missing = append(missing, "minio-secret-key / MINIO_SECRET_KEY")
	}
	if cfg.MinIOBucket == "" {
		missing = append(missing, "minio-bucket / MINIO_BUCKET")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
