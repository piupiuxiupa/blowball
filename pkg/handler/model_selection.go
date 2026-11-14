package handler

import (
	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/modelselect"
)

// ModelSelectionConfig remains the handler-facing name of the neutral catalog
// resolver configuration shared with scheduled-task services.
type ModelSelectionConfig = modelselect.Config

func NewModelSelectionConfig(cfg *config.Config) ModelSelectionConfig {
	return modelselect.NewConfig(cfg)
}

// modelSelection is the HTTP/queue adapter's resolved selection shape.
type modelSelection struct {
	Model               string
	Thinking            bool
	Vision              bool
	Effort              string
	MaxContextTokens    int
	MaxCompletionTokens int
	LengthContinue      config.LengthContinueConfig
}

func (s modelSelection) override() agent.ModelOverride {
	return agent.ModelOverride{
		Model:               s.Model,
		Thinking:            s.Thinking,
		ReasoningEffort:     s.Effort,
		MaxContextTokens:    s.MaxContextTokens,
		MaxCompletionTokens: s.MaxCompletionTokens,
		LengthContinue:      s.LengthContinue,
	}
}

func resolveModelSelection(msc ModelSelectionConfig, model, effort string) (modelSelection, string, string) {
	sel, code, msg := modelselect.Resolve(msc, model, effort)
	return modelSelection{
		Model:               sel.Model,
		Thinking:            sel.Thinking,
		Vision:              sel.Vision,
		Effort:              sel.Effort,
		MaxContextTokens:    sel.MaxContextTokens,
		MaxCompletionTokens: sel.MaxCompletionTokens,
		LengthContinue:      sel.LengthContinue,
	}, code, msg
}
