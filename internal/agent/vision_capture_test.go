package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pngVisionHeader is enough for the wire path (magic bytes are preserved
// verbatim; the client does not decode the image).
var pngVisionHeader = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3}

// TestVisionClient_RawCaptureAttribution pins the xizhi-view-image spec's
// capture requirement end-to-end at the client level: the vision sidecar call
// rides the shared StreamChat capture path, so llm_raw_log sees request +
// response rows attributed to the "vision" pseudo-agent with the triggering
// turn's trace/session/user correlation, and the request row carries the
// base64 data-URL image part verbatim.
func TestVisionClient_RawCaptureAttribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseSuccessBody))
	}))
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newCapturingClient(sink, srv.URL)

	vc, err := NewVisionClient(client, visionCatalog(false), "glm-4v-flash", 2048)
	require.NoError(t, err)

	ctx := trace.WithContext(context.Background(), "trace-vision")
	ctx = WithSessionID(ctx, "sess-vision")
	ctx = skill.WithUserID(ctx, "user-vision")
	desc, err := vc.Describe(ctx, "what is in this image?", pngVisionHeader, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "Hello", desc)

	recs := sink.snapshot()
	require.Len(t, recs, 2, "vision calls emit request and stitched response rows")
	for i, r := range recs {
		assert.Equal(t, "vision", r.Agent, "vision calls attribute to the vision pseudo-agent")
		assert.Equal(t, "trace-vision", r.TraceID)
		assert.Equal(t, "sess-vision", r.SessionID)
		assert.Equal(t, "user-vision", r.UserID)
		assert.Equal(t, "glm-4v-flash", r.Model)
		assert.Equal(t, i, r.FrameIndex)
	}
	assert.Equal(t, rawKindRequest, recs[0].Kind)
	assert.Equal(t, rawKindResponse, recs[1].Kind)

	var sent struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(recs[0].Raw), &sent))
	require.Len(t, sent.Messages, 1)
	require.Len(t, sent.Messages[0].Content, 2)
	assert.Equal(t, "text", sent.Messages[0].Content[0]["type"])
	assert.Equal(t, "what is in this image?", sent.Messages[0].Content[0]["text"])
	assert.Equal(t, "image_url", sent.Messages[0].Content[1]["type"])
	imageURL, ok := sent.Messages[0].Content[1]["image_url"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, imageURL["url"], "data:image/png;base64,", "the captured request row keeps the base64 image payload verbatim (never truncated)")
}
