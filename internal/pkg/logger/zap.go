// Package logger initializes the application-wide zap logger.
//
// Init returns a production-style zap.Logger that writes JSON to stdout with a
// configurable level. A package-level default logger is exposed via L() so
// callers that do not yet hold a *zap.Logger can still emit logs.
//
// Per-request fields such as trace_id are intended to be attached through
// logger.With(zap.String("trace_id", id)) when a context-aware logger is
// threaded through the call chain; the global L() remains context-free.
package logger

import (
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	mu      sync.RWMutex
	defaultLogger = zap.NewNop()
)

// Init builds a production-style JSON zap.Logger writing to stdout at the
// given level. Level must be one of debug, info, warn, error (case-insensitive)
// or an empty string (defaults to info). It also stores the logger as the
// package default returned by L().
func Init(level string) (*zap.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.Encoding = "json"
	cfg.EncoderConfig.TimeKey = "timestamp"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	logger, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build zap logger: %w", err)
	}

	mu.Lock()
	defaultLogger = logger
	mu.Unlock()

	return logger, nil
}

// L returns the package-level default logger. Before Init is called it returns
// a no-op logger so callers can call L() safely at any time.
func L() *zap.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return defaultLogger
}

// SetDefault replaces the package-level default logger. Intended for tests and
// bootstrap paths that construct a logger directly.
func SetDefault(l *zap.Logger) {
	mu.Lock()
	defaultLogger = l
	mu.Unlock()
}

// parseLevel maps a human-readable level string to zapcore.Level.
func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q (want debug|info|warn|error)", s)
	}
}
