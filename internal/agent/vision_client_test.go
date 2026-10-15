package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/lush/blowball/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVisionLLM captures the LLMRequest handed to StreamChat so the vision
// adapter can be asserted without a network.
type fakeVisionLLM struct {
	gotReq   LLMRequest
	gotAgent string
	resp     LLMResponse
	err      error
}

func (f *fakeVisionLLM) StreamChat(ctx context.Context, req LLMRequest, _ func(string) error, _ func(string) error) (LLMResponse, error) {
	f.gotReq = req
	f.gotAgent = AgentNameFromContext(ctx)
	if f.err != nil {
		return LLMResponse{}, f.err
	}
	return f.resp, nil
}

func visionCatalog(thinking bool) config.OpenAIConfig {
	return config.OpenAIConfig{
		Models: []config.ModelCatalogEntry{
			{Name: "glm-4v-flash", Thinking: thinking},
		},
	}
}

func TestNewVisionClient_ResolvesCatalogEntry(t *testing.T) {
	vc, err := NewVisionClient(&fakeVisionLLM{}, visionCatalog(false), "glm-4v-flash", 1234)
	require.NoError(t, err)
	require.NotNil(t, vc)

	_, err = NewVisionClient(&fakeVisionLLM{}, visionCatalog(false), "missing", 1234)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
}

func TestVisionClient_DescribeBuildsSidecarRequest(t *testing.T) {
	llm := &fakeVisionLLM{resp: LLMResponse{Content: "a chart showing 42%"}}
	vc, err := NewVisionClient(llm, visionCatalog(false), "glm-4v-flash", 2048)
	require.NoError(t, err)

	img := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2}
	got, err := vc.Describe(context.Background(), "what does this chart show?", img, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "a chart showing 42%", got)

	assert.Equal(t, "glm-4v-flash", llm.gotReq.Model)
	assert.Equal(t, 2048, llm.gotReq.MaxCompletionTokens)
	assert.False(t, llm.gotReq.Thinking)
	require.Len(t, llm.gotReq.Messages, 1)
	assert.Equal(t, "user", llm.gotReq.Messages[0].Role)
	assert.Equal(t, "what does this chart show?", llm.gotReq.Messages[0].Content)
	require.NotNil(t, llm.gotReq.vision)
	assert.Equal(t, "image/png", llm.gotReq.vision.MIMEType)
	assert.Equal(t, img, llm.gotReq.vision.Data)
	assert.Equal(t, "vision", llm.gotAgent, "raw-capture attribution must be the vision pseudo-agent")
}

func TestVisionClient_ThinkingEntryRequestsNoneEffort(t *testing.T) {
	llm := &fakeVisionLLM{resp: LLMResponse{Content: "ok"}}
	vc, err := NewVisionClient(llm, visionCatalog(true), "glm-4v-flash", 64)
	require.NoError(t, err)

	_, err = vc.Describe(context.Background(), "q", []byte("x"), "image/png")
	require.NoError(t, err)
	assert.True(t, llm.gotReq.Thinking)
	assert.Equal(t, "none", llm.gotReq.ReasoningEffort)
}

func TestVisionClient_DescribeSurfacesModelError(t *testing.T) {
	llm := &fakeVisionLLM{err: assert.AnError}
	vc, err := NewVisionClient(llm, visionCatalog(false), "glm-4v-flash", 64)
	require.NoError(t, err)

	_, err = vc.Describe(context.Background(), "q", []byte("x"), "image/png")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vision model call")
}

// TestOpenAIClient_VisionRequestRendersImageParts pins the wire shape of the
// vision side channel: the user message becomes a content-parts array with the
// question as a text part followed by the image as a base64 data-URL part.
func TestOpenAIClient_VisionRequestRendersImageParts(t *testing.T) {
	img := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 9}
	body := captureRequestBody(t, LLMRequest{
		Model:               "glm-4v-flash",
		Messages:            []Message{{Role: "user", Content: "what is the error stack?"}},
		MaxCompletionTokens: 512,
		vision:              &visionImage{MIMEType: "image/png", Data: img},
	})

	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Messages, 1)
	assert.Equal(t, "user", parsed.Messages[0].Role)

	var parts []map[string]any
	require.NoError(t, json.Unmarshal(parsed.Messages[0].Content, &parts), "vision user content must be a parts array, got %s", parsed.Messages[0].Content)
	require.Len(t, parts, 2)
	assert.Equal(t, "text", parts[0]["type"])
	assert.Equal(t, "what is the error stack?", parts[0]["text"])
	assert.Equal(t, "image_url", parts[1]["type"])
	imageURL, _ := parts[1]["image_url"].(map[string]any)
	require.NotNil(t, imageURL)
	assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(img), imageURL["url"])
	assert.Contains(t, body, `"max_completion_tokens":512`)
	assert.Contains(t, body, `"max_tokens":512`, "non-thinking vision entries keep both quota spellings")
}
