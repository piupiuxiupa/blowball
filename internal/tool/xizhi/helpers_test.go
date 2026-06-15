package xizhi

import (
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

// newTestRegistry returns a fresh *tool.Registry for use within the xizhi
// package tests (mostly to exercise the Execute callbacks that RegisterAll
// wires up).
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry()
}

// testXizhiConfig returns a XizhiConfig with all tools enabled for tests.
func testXizhiConfig() config.XizhiConfig {
	return config.XizhiConfig{
		Read:      config.XizhiToolConfig{Enabled: true},
		Write:     config.XizhiToolConfig{Enabled: true},
		Modify:    config.XizhiToolConfig{Enabled: true},
		ListFiles: config.XizhiToolConfig{Enabled: true},
		Tree:      config.XizhiToolConfig{Enabled: true},
		GlobFiles: config.XizhiToolConfig{Enabled: true},
	}
}
