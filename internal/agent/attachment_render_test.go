package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToOpenAIMessage_UserImages_TextFirst(t *testing.T) {
	img1 := []byte("fake-png-bytes-1")
	img2 := []byte("fake-png-bytes-2")
	m := Message{
		Role:    "user",
		Content: "what is in these images?",
		Images: []MessageImage{
			{AttachmentID: "a-1", MIMEType: "image/png", Data: img1},
			{AttachmentID: "a-2", MIMEType: "image/webp", Data: img2},
		},
	}

	raw, err := json.Marshal(toOpenAIMessage(m))
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	content, ok := doc["content"].([]any)
	require.True(t, ok, "image-bearing user message must render content parts")
	require.Len(t, content, 3)

	text, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "what is in these images?", text["text"])

	mimes := []string{"image/png", "image/webp"}
	for i, data := range [][]byte{img1, img2} {
		part, ok := content[i+1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "image_url", part["type"])
		iu, ok := part["image_url"].(map[string]any)
		require.True(t, ok)
		want := "data:" + mimes[i] + ";base64," + base64.StdEncoding.EncodeToString(data)
		assert.Equal(t, want, iu["url"])
	}
}

func TestToOpenAIMessage_PlainUserUnchanged(t *testing.T) {
	raw, err := json.Marshal(toOpenAIMessage(Message{Role: "user", Content: "plain"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"role":"user","content":"plain"}`, string(raw))
}

func TestStripImages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q", Images: []MessageImage{{AttachmentID: "a", MIMEType: "image/png", Data: []byte("x")}}},
		{Role: "assistant", Content: "a"},
	}
	out := StripImages(msgs)
	require.Len(t, out, 2)
	assert.Empty(t, out[0].Images)
	assert.Equal(t, "q", out[0].Content)

	// No-images input returns the same slice value unchanged (no copy churn).
	plain := []Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}
	stripped := StripImages(plain)
	require.Len(t, stripped, 2)
	assert.Equal(t, plain[0].Content, stripped[0].Content)
}

func TestCaptureRequestJSON_RedactsAtBuildTime(t *testing.T) {
	imgData := []byte("secret-image-bytes")
	msgs := []Message{
		{Role: "user", Content: "hello", Images: []MessageImage{
			{AttachmentID: "att-1", MIMEType: "image/png", Data: imgData},
		}},
		{Role: "assistant", Content: "hi"},
	}
	params := openai.ChatCompletionNewParams{
		Model:    "m",
		Messages: toOpenAIMessages(msgs),
	}

	out, err := captureRequestJSON(LLMRequest{Model: "m", Messages: msgs}, params)
	require.NoError(t, err)

	s := string(out)
	assert.NotContains(t, s, "base64,")
	assert.Contains(t, s, `"type":"image_url_redacted"`)
	assert.Contains(t, s, `"attachment_id":"att-1"`)
	assert.Contains(t, s, `"bytes":18`)
	sum := sha256.Sum256(imgData)
	assert.Contains(t, s, fmt.Sprintf(`"sha256":"%x"`, sum))
	assert.Contains(t, s, `"text":"hello"`, "text parts stay verbatim")
	assert.Contains(t, s, `"assistant"`)
	assert.Contains(t, s, `"model":"m"`, "non-message wire fields survive the shadow struct")

	// No images → the wire params marshal directly (byte-identical capture).
	plainMsgs := []Message{{Role: "user", Content: "plain"}}
	plainParams := openai.ChatCompletionNewParams{Model: "m", Messages: toOpenAIMessages(plainMsgs)}
	direct, err := json.Marshal(plainParams)
	require.NoError(t, err)
	viaCapture, err := captureRequestJSON(LLMRequest{Messages: plainMsgs}, plainParams)
	require.NoError(t, err)
	assert.Equal(t, string(direct), string(viaCapture))
}

func TestCaptureRequestJSON_VisionSideChannelImageRedacted(t *testing.T) {
	img := []byte("vision-bytes")
	msgs := []Message{{Role: "user", Content: "what is this?"}}
	params := openai.ChatCompletionNewParams{Model: "vm", Messages: toOpenAIMessages(msgs)}
	params.Messages = withVisionImage(params.Messages, &visionImage{MIMEType: "image/png", Data: img})

	out, err := captureRequestJSON(LLMRequest{Messages: msgs, vision: &visionImage{MIMEType: "image/png", Data: img}}, params)
	require.NoError(t, err)

	s := string(out)
	assert.NotContains(t, s, "base64,")
	assert.NotContains(t, s, "attachment_id", "the side channel has no attachment identity")
	assert.Contains(t, s, `"type":"image_url_redacted"`)
	assert.Contains(t, s, `"bytes":12`)
	assert.Contains(t, s, `"text":"what is this?"`)
}
