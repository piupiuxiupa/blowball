package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool/webfetch"
)

// webfetchPromptClient adapts webfetch's narrow digest prompt interface to the
// shared production LLM client. Keeping this adapter in agent prevents the tool
// package from importing agent and creating a dependency cycle.
type webfetchPromptClient struct {
	llm   LLMClient
	entry config.ModelCatalogEntry
}

// NewWebfetchPromptClient resolves model in the mandatory catalog and returns the
// prompt client used by webfetch.NewDigester.
func NewWebfetchPromptClient(llm LLMClient, openAI config.OpenAIConfig, model string) (webfetch.PromptClient, error) {
	name := model
	if name == "" {
		name = openAI.DefaultModel
		if name == "" && len(openAI.Models) > 0 {
			name = openAI.Models[0].Name
		}
	}
	entry, ok := openAI.FindModelCatalogEntry(name)
	if !ok {
		return nil, fmt.Errorf("agent: webfetch digest model %q not found in openai.models", name)
	}
	return webfetchPromptClient{llm: llm, entry: entry}, nil
}

// Prompt executes a non-streaming-style chat completion through the shared
// streaming client. Digest calls always request the non-reasoning effort family
// when a thinking model is selected, keeping this side channel inexpensive.
func (c webfetchPromptClient) Prompt(ctx context.Context, system, user string, maxOutputTokens int) (string, error) {
	if c.llm == nil {
		return "", fmt.Errorf("agent: webfetch digest llm client is nil")
	}
	ctx = WithAgentName(ctx, "webfetch_digest")
	req := LLMRequest{
		Model: c.entry.Name,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxCompletionTokens: maxOutputTokens,
	}
	if c.entry.Thinking {
		req.Thinking = true
		req.ReasoningEffort = "none"
	}
	resp, err := c.llm.StreamChat(ctx, req, nil, nil)
	if err != nil {
		return "", fmt.Errorf("agent: webfetch digest model call: %w", err)
	}
	return resp.Content, nil
}
