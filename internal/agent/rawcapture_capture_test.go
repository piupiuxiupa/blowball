package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
)

// fakeCaptureSink records every capture for assertions.
type fakeCaptureSink struct {
	mu      sync.Mutex
	records []RawCaptureRecord
}

func (f *fakeCaptureSink) Capture(_ context.Context, rec RawCaptureRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
}

func (f *fakeCaptureSink) snapshot() []RawCaptureRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RawCaptureRecord, len(f.records))
	copy(out, f.records)
	return out
}

// newCapturingClient builds an OpenAIClient pointed at srv with sink attached.
func newCapturingClient(sink *fakeCaptureSink, baseURL string) *OpenAIClient {
	return &OpenAIClient{
		client: openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(baseURL+"/v1")),
		sink:   sink,
	}
}

// captureTestCtx returns a context decorated with the full attribution set.
func captureTestCtx() context.Context {
	ctx := trace.WithContext(context.Background(), "trace-cap")
	ctx = WithSessionID(ctx, "sess-cap")
	ctx = WithAgentName(ctx, "Chongzhi")
	return skill.WithUserID(ctx, "user-cap")
}

const sseSuccessBody = `data: {"id":"chatcmpl-cap","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

data: {"id":"chatcmpl-cap","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}

data: {"id":"chatcmpl-cap","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}

data: [DONE]

`

func TestStreamChatCapture_SuccessPair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseSuccessBody))
	}))
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newCapturingClient(sink, srv.URL)

	resp, err := client.StreamChat(captureTestCtx(), LLMRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)

	// sseSuccessBody carries 3 SSE frames (+ the [DONE] sentinel, which is
	// not a chunk): request(0) + chunk(1..3) + response(4).
	recs := sink.snapshot()
	require.Len(t, recs, 5)

	req, res := recs[0], recs[4]

	// Pairing and attribution shared by every row of the call.
	assert.Equal(t, req.CallID, res.CallID)
	assert.Equal(t, req.Seq, res.Seq)
	assert.Greater(t, req.Seq, 0)
	for i, r := range recs {
		assert.Equal(t, "trace-cap", r.TraceID)
		assert.Equal(t, "sess-cap", r.SessionID)
		assert.Equal(t, "user-cap", r.UserID)
		assert.Equal(t, "Chongzhi", r.Agent)
		assert.Equal(t, "gpt-test", r.Model)
		assert.Equal(t, i, r.FrameIndex, "frame_index must order records within the call")
	}

	// Request row: the as-sent params JSON.
	assert.Equal(t, rawKindRequest, req.Kind)
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(req.Raw), &sent))
	assert.Equal(t, "gpt-test", sent["model"])
	msgs, ok := sent["messages"].([]any)
	require.True(t, ok, "as-sent request must carry the messages array")
	require.Len(t, msgs, 1)
	assert.Equal(t, 0, res.HTTPStatus)

	// Chunk rows: verbatim wire bytes, in arrival order.
	frame1 := `{"id":"chatcmpl-cap","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`
	for i, want := range []string{frame1, recs[2].Raw, recs[3].Raw} {
		c := recs[1+i]
		assert.Equal(t, rawKindChunk, c.Kind)
		if i == 0 {
			assert.Equal(t, want, c.Raw, "chunk raw must be the frame's wire bytes, verbatim")
		}
		var frame map[string]any
		require.NoError(t, json.Unmarshal([]byte(c.Raw), &frame), "each chunk raw must parse as one chat.completion.chunk")
	}
	// The three frames concatenated inside their delta.content reproduce the reply.
	assert.Contains(t, recs[1].Raw, `"Hel"`)
	assert.Contains(t, recs[2].Raw, `"lo"`)

	// Response row: the stitched non-streaming-equivalent completion.
	assert.Equal(t, rawKindResponse, res.Kind)
	assert.Equal(t, "stop", res.FinishReason)
	assert.GreaterOrEqual(t, res.DurationMS, int64(0))
	var completion rawCaptureCompletion
	require.NoError(t, json.Unmarshal([]byte(res.Raw), &completion))
	assert.Equal(t, "chatcmpl-cap", completion.ID)
	assert.Equal(t, "chat.completion", completion.Object)
	assert.Equal(t, "gpt-test", completion.Model)
	require.Len(t, completion.Choices, 1)
	assert.Equal(t, "Hello", completion.Choices[0].Message.Content)
	assert.Equal(t, "stop", completion.Choices[0].FinishReason)
	require.NotNil(t, completion.Usage)
	assert.Equal(t, 9, completion.Usage.TotalTokens)
}

func TestStreamChatCapture_GatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"thinking budget too large","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newCapturingClient(sink, srv.URL)

	_, err := client.StreamChat(captureTestCtx(), LLMRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil, nil)
	require.Error(t, err)

	recs := sink.snapshot()
	require.Len(t, recs, 2, "failed call must produce a request + error pair (no frames arrived)")

	req, errRow := recs[0], recs[1]
	assert.Equal(t, rawKindRequest, req.Kind)
	assert.Equal(t, req.CallID, errRow.CallID)
	assert.Equal(t, req.Seq, errRow.Seq)
	assert.Equal(t, 0, req.FrameIndex)
	assert.Equal(t, 1, errRow.FrameIndex)

	assert.Equal(t, rawKindError, errRow.Kind)
	assert.Equal(t, http.StatusBadRequest, errRow.HTTPStatus)
	assert.Contains(t, errRow.Raw, "thinking budget too large",
		"error row raw must carry the gateway's error body")
}

func TestStreamChatCapture_CancelMidStream(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// First content chunk, then hold the stream open until the client
		// disconnects.
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-cap","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}

`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newCapturingClient(sink, srv.URL)

	ctx, cancel := context.WithCancel(captureTestCtx())
	// Cancel from inside onToken: after the first delta arrives the stream is
	// torn down mid-flight.
	_, err := client.StreamChat(ctx, LLMRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(_ string) error {
		cancel()
		return nil
	}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	// Deadline for the async capture to settle (the SDK surfaces cancellation
	// on its own schedule). One frame arrived: request(0) + chunk(1) +
	// partial response(2).
	require.Eventually(t, func() bool {
		return len(sink.snapshot()) == 3
	}, 5*time.Second, 20*time.Millisecond, "cancel path must produce request + arrived chunk + partial-response rows")

	recs := sink.snapshot()
	req, res := recs[0], recs[2]
	assert.Equal(t, rawKindRequest, req.Kind)
	assert.Equal(t, req.CallID, res.CallID)
	assert.Equal(t, rawKindChunk, recs[1].Kind)
	assert.Contains(t, recs[1].Raw, "partial")
	assert.Equal(t, rawKindResponse, res.Kind, "local cancellation is a partial response row, not an error row")
	assert.Equal(t, 0, res.HTTPStatus)

	var completion rawCaptureCompletion
	require.NoError(t, json.Unmarshal([]byte(res.Raw), &completion))
	require.Len(t, completion.Choices, 1)
	assert.Equal(t, "partial", completion.Choices[0].Message.Content)
}

func TestStreamChatCapture_FrameBudgetTruncation(t *testing.T) {
	// Nine ~1MB frames cross the 8MB per-call frame budget mid-stream: frame
	// capture stops, a {"_truncated":true,...} marker row lands, and the
	// stitched response row still lands.
	bigPayload := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 9; i++ {
			frame := fmt.Sprintf(`data: {"id":"chatcmpl-big","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`+"\n\n",
				mustJSON(bigPayload))
			_, _ = w.Write([]byte(frame))
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-big","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newCapturingClient(sink, srv.URL)

	resp, err := client.StreamChat(captureTestCtx(), LLMRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil, nil)
	require.NoError(t, err)

	recs := sink.snapshot()
	require.NotEmpty(t, recs)

	var chunks, markers int
	var lastChunkIdx int
	for _, r := range recs {
		switch r.Kind {
		case rawKindChunk:
			if strings.Contains(r.Raw, `"_truncated":true`) {
				markers++
				var m map[string]any
				require.NoError(t, json.Unmarshal([]byte(r.Raw), &m))
				assert.Equal(t, true, m["_truncated"])
				assert.Equal(t, "frame_capture_cap", m["reason"])
				assert.Greater(t, m["bytes"], float64(frameCaptureCap-1<<20))
			} else {
				chunks++
				lastChunkIdx = r.FrameIndex
			}
		}
	}
	assert.Equal(t, 1, markers, "exactly one truncation marker row")
	// The budget check runs after the frame that crosses it: 8 ~1MB frames
	// accumulate past 8MB, the marker lands, and frame 9 is never captured.
	assert.Equal(t, 8, chunks, "frames up to and including the budget-crossing one must be captured")
	// The response row sits after the last chunk/marker record.
	var res *RawCaptureRecord
	for i := range recs {
		if recs[i].Kind == rawKindResponse {
			res = &recs[i]
		}
	}
	require.NotNil(t, res, "response row must survive frame truncation")
	assert.Equal(t, lastChunkIdx+2, res.FrameIndex, "response row follows the marker row")
	assert.NotEmpty(t, resp.Content)
}

func TestStreamChatCapture_NilSinkZeroBehavior(t *testing.T) {
	// A client built without a sink must behave exactly as before: no panic,
	// no capture, same response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseSuccessBody))
	}))
	defer srv.Close()

	client := NewOpenAIClientFromClient(openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/v1"),
	), 0)
	resp, err := client.StreamChat(captureTestCtx(), LLMRequest{
		Model:    "gpt-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)
}
