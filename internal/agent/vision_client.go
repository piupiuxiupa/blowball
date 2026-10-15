package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// visionClient adapts xizhi's narrow vision interface to the shared production
// LLM client. Keeping this adapter in agent prevents the tool package from
// importing agent and creating a dependency cycle (same shape as
// webfetchPromptClient).
type visionClient struct {
	llm             LLMClient
	entry           config.ModelCatalogEntry
	maxOutputTokens int
}

// NewVisionClient resolves the configured vision model in the mandatory
// catalog and returns the client used by xizhi.RegisterAll for the
// xizhi_view_image tool. maxOutputTokens bounds the description length.
func NewVisionClient(llm LLMClient, openAI config.OpenAIConfig, model string, maxOutputTokens int) (xizhi.VisionClient, error) {
	entry, ok := openAI.FindModelCatalogEntry(model)
	if !ok {
		return nil, fmt.Errorf("agent: vision model %q not found in openai.models", model)
	}
	return visionClient{llm: llm, entry: entry, maxOutputTokens: maxOutputTokens}, nil
}

// Describe sends the image plus the (possibly defaulted) question to the
// configured vision model as one base64-inline user message and returns the
// model's text description. The call rides the shared streaming client, so
// llm-raw-capture observes it for free; attribution is the "vision"
// pseudo-agent (like compaction's "compaction"). Vision calls always request
// the non-reasoning effort family when a thinking model is selected, keeping
// this side channel inexpensive.
func (c visionClient) Describe(ctx context.Context, question string, image []byte, mime string) (string, error) {
	if c.llm == nil {
		return "", fmt.Errorf("agent: vision llm client is nil")
	}
	ctx = WithAgentName(ctx, "vision")
	req := LLMRequest{
		Model: c.entry.Name,
		Messages: []Message{
			{Role: "user", Content: question},
		},
		MaxCompletionTokens: c.maxOutputTokens,
		vision:              &visionImage{MIMEType: mime, Data: image},
	}
	if c.entry.Thinking {
		req.Thinking = true
		req.ReasoningEffort = "none"
	}
	resp, err := c.llm.StreamChat(ctx, req, nil, nil)
	if err != nil {
		return "", fmt.Errorf("agent: vision model call: %w", err)
	}
	return resp.Content, nil
}
