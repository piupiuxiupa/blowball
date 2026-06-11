package logger

import (
	"testing"

	"go.uber.org/zap"
)

func TestInit_LevelsReturnNonNilLogger(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", ""} {
		t.Run(lvl, func(t *testing.T) {
			l, err := Init(lvl)
			if err != nil {
				t.Fatalf("Init(%q) returned error: %v", lvl, err)
			}
			if l == nil {
				t.Fatalf("Init(%q) returned nil logger", lvl)
			}
			// Sanity: logger should be usable without panicking.
			l.Info("smoke test", zap.String("level", lvl))
		})
	}
}

func TestInit_InvalidLevelReturnsError(t *testing.T) {
	if _, err := Init("verbose"); err == nil {
		t.Fatal("Init(invalid) expected error, got nil")
	}
}

func TestL_ReturnsLoggerAfterInit(t *testing.T) {
	// Ensure default L() is non-nil and safe even before Init.
	if L() == nil {
		t.Fatal("L() returned nil before Init")
	}

	l, err := Init("debug")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if L() == nil {
		t.Fatal("L() returned nil after Init")
	}

	// Restore to a fresh nop logger to avoid leaking state into other tests.
	SetDefault(zap.NewNop())
	_ = l
}
