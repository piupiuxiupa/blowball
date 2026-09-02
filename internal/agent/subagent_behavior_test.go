package agent

import (
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

// newGenericFromAgentConfig adapts the historical leaf-agent test fixtures to
// the one generic implementation. Production constructs SubAgentSpec directly.
func newGenericFromAgentConfig(cfg config.AgentConfig, client LLMClient, reg *tool.Registry, turn ModelOverride) (*SubAgent, error) {
	return NewSubAgent(SubAgentSpec{
		Name:         cfg.Name,
		SystemPrompt: cfg.SystemPrompt,
		Tools:        cfg.Tools,
		ToolRegistry: reg,
		MaxRounds:    cfg.MaxRounds,
		Retry:        cfg.Retry,
		OutputSchema: cfg.OutputSchema,
		Turn:         turn,
	}, client)
}
