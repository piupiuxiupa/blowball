package xizhi

import (
	"testing"

	"github.com/lush/blowball/internal/tool"
)

// newTestRegistry returns a fresh *tool.Registry for use within the xizhi
// package tests (mostly to exercise the Execute callbacks that RegisterAll
// wires up).
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry()
}
