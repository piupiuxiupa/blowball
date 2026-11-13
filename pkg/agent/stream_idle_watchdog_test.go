package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIdleWatchdogClient builds an OpenAIClient with the stream idle watchdog
// armed at idle, pointed at baseURL. A nil sink disables raw capture.
func newIdleWatchdogClient(sink RawCaptureSink, baseURL string, idle time.Duration) *OpenAIClient {
	return &OpenAIClient{
		client:            openai.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(baseURL+"/v1")),
		sink:              sink,
		streamIdleTimeout: idle,
	}
}

// idleFrame builds one SSE data frame carrying a content delta.
func idleFrame(id, content string) string {
	return fmt.Sprintf(`data: {"id":%s,"object":"chat.completion.chunk","created":1700000000,"model":"gpt-watch","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`+"\n\n",
		mustJSON(id), mustJSON(content))
}

// idleStopFrame is the closing frame with finish_reason=stop.
const idleStopFrame = `data: {"id":"chatcmpl-watch","object":"chat.completion.chunk","created":1700000000,"model":"gpt-watch","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

// newStallingSrv serves a stream that emits n frames and then goes silent
// with the connection held open — the 2026-08-18 incident shape.
func newStallingSrv(t *testing.T, n int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < n; i++ {
			_, _ = w.Write([]byte(idleFrame("chatcmpl-watch", "x")))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done() // stall: connection open, zero bytes
	}))
}

// callStreamChat runs StreamChat with a hang guard: if it does not return
// within 5s the test fails immediately instead of hanging the suite — the
// exact regression the watchdog exists to prevent.
func callStreamChat(t *testing.T, client *OpenAIClient, ctx context.Context) (LLMResponse, error) {
	t.Helper()
	type result struct {
		resp LLMResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.StreamChat(ctx, LLMRequest{
			Model:    "gpt-watch",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}, nil, nil)
		done <- result{resp: resp, err: err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("StreamChat did not return within 5s; the idle watchdog failed to abort the stalled stream")
		return LLMResponse{}, nil // unreachable
	}
}

// TestStreamIdleWatchdog_MidStreamStall covers the incident scenario: frames
// flow, then the gateway goes silent with the connection open. The watchdog
// must abort the call with the typed error carrying frames received and the
// model name (llm-stream-watchdog spec: "Stream stalls mid-generation").
func TestStreamIdleWatchdog_MidStreamStall(t *testing.T) {
	srv := newStallingSrv(t, 2)
	defer srv.Close()

	client := newIdleWatchdogClient(nil, srv.URL, 120*time.Millisecond)
	_, err := callStreamChat(t, client, context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStreamIdleTimeout)
	msg := err.Error()
	assert.Contains(t, msg, "timeout", "the message must keep the transient-classifier substring")
	assert.Contains(t, msg, "frames=2", "the message must carry the frames-received diagnostic")
	assert.Contains(t, msg, "model=gpt-watch")
	assert.Contains(t, msg, (120 * time.Millisecond).String(), "the message must carry the configured idle")
}

// TestStreamIdleWatchdog_FirstFrameNeverArrives covers time-to-first-frame:
// the HTTP handshake succeeds but zero frames arrive. The first gap starts at
// call issuance, so the watchdog fires (spec: "Gateway never sends the first
// frame").
func TestStreamIdleWatchdog_FirstFrameNeverArrives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers out, then silence
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newIdleWatchdogClient(nil, srv.URL, 100*time.Millisecond)
	_, err := callStreamChat(t, client, context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStreamIdleTimeout)
	assert.Contains(t, err.Error(), "frames=0")
	assert.Contains(t, err.Error(), "model=gpt-watch")
}

// TestStreamIdleWatchdog_LongButAliveStreamNeverAborted covers the non-goal
// guard: only inter-frame silence, never total duration, triggers the
// watchdog. Frames arrive every 40ms for well over the 250ms idle window and
// the call must complete normally (spec: "Long but alive generation is never
// aborted").
func TestStreamIdleWatchdog_LongButAliveStreamNeverAborted(t *testing.T) {
	const idle = 250 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 10; i++ {
			_, _ = w.Write([]byte(idleFrame("chatcmpl-watch", fmt.Sprintf("%d", i))))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond) // 10 frames ≈ 400ms total ≫ 250ms idle
		}
		_, _ = w.Write([]byte(idleStopFrame))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := newIdleWatchdogClient(nil, srv.URL, idle)
	resp, err := callStreamChat(t, client, context.Background())
	require.NoError(t, err)
	assert.Equal(t, "0123456789", resp.Content)
	assert.Equal(t, "stop", resp.FinishReason)
}

// TestStreamIdleWatchdog_DisabledKeepsPriorBehavior covers the opt-in guard:
// with idle=0 a stalled stream blocks until the caller's context cancels —
// exactly the pre-capability semantics, and no idle error is ever produced
// (spec: "Unset config keeps prior behavior").
func TestStreamIdleWatchdog_DisabledKeepsPriorBehavior(t *testing.T) {
	srv := newStallingSrv(t, 1)
	defer srv.Close()

	client := newIdleWatchdogClient(nil, srv.URL, 0) // disabled
	// Give a would-be-armed watchdog (100ms, well inside the bound below) every
	// chance to misfire: the only thing that may unblock the read loop is the
	// parent context.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := callStreamChat(t, client, ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrStreamIdleTimeout)
}

// TestStreamIdleWatchdog_ParentCancelPrecedence covers design D3: when the
// parent context and the idle timer expire at the same moment, the call
// reports the caller's cancellation — never the idle error — with the
// existing partial-response capture behavior (spec: "Client disconnects while
// the timer fires").
func TestStreamIdleWatchdog_ParentCancelPrecedence(t *testing.T) {
	srv := newStallingSrv(t, 1)
	defer srv.Close()

	client := newIdleWatchdogClient(nil, srv.URL, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel) // same deadline as the idle timer
	_, err := callStreamChat(t, client, ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrStreamIdleTimeout)
}

// TestStreamIdleWatchdog_GoroutineExitsOnAllPaths guards the leak invariant:
// the watchdog goroutine selects on callCtx.Done() and cancel is deferred, so
// it must not outlive its call on the normal-completion, watchdog-error, and
// parent-cancel paths (spec: "The watchdog goroutine MUST NOT outlive its
// call").
func TestStreamIdleWatchdog_GoroutineExitsOnAllPaths(t *testing.T) {
	baseline := runtime.NumGoroutine()

	scenarios := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"normal completion", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(idleFrame("chatcmpl-watch", "ok")))
				_, _ = w.Write([]byte(idleStopFrame))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer srv.Close()
			_, err := callStreamChat(t, newIdleWatchdogClient(nil, srv.URL, time.Second), context.Background())
			require.NoError(t, err)
		}},
		{"watchdog error", func(t *testing.T) {
			srv := newStallingSrv(t, 1)
			defer srv.Close()
			_, err := callStreamChat(t, newIdleWatchdogClient(nil, srv.URL, 80*time.Millisecond), context.Background())
			require.ErrorIs(t, err, ErrStreamIdleTimeout)
		}},
		{"parent cancel", func(t *testing.T) {
			srv := newStallingSrv(t, 1)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			_, err := callStreamChat(t, newIdleWatchdogClient(nil, srv.URL, 10*time.Minute), ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t)
			require.Eventually(t, func() bool {
				return runtime.NumGoroutine() <= baseline+2
			}, 5*time.Second, 25*time.Millisecond,
				"watchdog goroutine must exit with its call (leak)")
		})
	}
}

// TestErrStreamIdleTimeout_TransientClassified guards the zero-change retry
// integration contract: the typed error's message carries "timeout", so the
// existing substring classifier keeps treating a wrapped idle-timeout failure
// as retryable (llm-stream-watchdog spec: "Idle-timeout errors are
// transient-classified for sub-agent retry").
func TestErrStreamIdleTimeout_TransientClassified(t *testing.T) {
	assert.True(t, isTransientError(ErrStreamIdleTimeout))
	// The shape the agent loop actually produces: SDK/agent wrapping on top.
	wrapped := fmt.Errorf("liang: stream chat: %w: no frame for 2m0s (frames=3, model=m)",
		ErrStreamIdleTimeout)
	assert.True(t, isTransientError(wrapped))
}

// TestStreamIdleWatchdog_CaptureErrorRow covers the llm-raw-capture delta: a
// watchdog abort ends as a kind=error row with http_status=0 and the typed
// message (idle, frames, model) as the terminal raw body of the same call.
func TestStreamIdleWatchdog_CaptureErrorRow(t *testing.T) {
	srv := newStallingSrv(t, 2)
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newIdleWatchdogClient(sink, srv.URL, 120*time.Millisecond)
	_, err := callStreamChat(t, client, captureTestCtx())
	require.ErrorIs(t, err, ErrStreamIdleTimeout)

	// request(0) + error(1); async settle for the terminal row.
	require.Eventually(t, func() bool {
		return len(sink.snapshot()) == 2
	}, 5*time.Second, 20*time.Millisecond)

	recs := sink.snapshot()
	reqRow, errRow := recs[0], recs[1]
	assert.Equal(t, rawKindRequest, reqRow.Kind)
	assert.Equal(t, rawKindError, errRow.Kind, "watchdog abort is an error row, not a partial response row")
	assert.Equal(t, 0, errRow.HTTPStatus)
	assert.Equal(t, 1, errRow.FrameIndex, "error row is the terminal record")
	for _, r := range recs {
		assert.Equal(t, reqRow.CallID, r.CallID)
		assert.Equal(t, reqRow.Seq, r.Seq)
	}
	assert.Contains(t, errRow.Raw, "stream idle timeout")
	assert.Contains(t, errRow.Raw, "frames=2")
	assert.Contains(t, errRow.Raw, "model=gpt-watch")
	assert.Contains(t, errRow.Raw, (120 * time.Millisecond).String())
}

// TestStreamIdleWatchdog_CaptureParentCancelStaysPartial covers the other arm
// of the llm-raw-capture delta: with the watchdog armed, a caller cancellation
// still produces the partial kind=response row — no error row is added.
func TestStreamIdleWatchdog_CaptureParentCancelStaysPartial(t *testing.T) {
	srv := newStallingSrv(t, 1)
	defer srv.Close()

	sink := &fakeCaptureSink{}
	client := newIdleWatchdogClient(sink, srv.URL, 10*time.Minute) // armed but far away
	ctx, cancel := context.WithCancel(captureTestCtx())
	time.AfterFunc(150*time.Millisecond, cancel)
	_, err := callStreamChat(t, client, ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	// request(0) + partial response(1); async settle for the final row.
	require.Eventually(t, func() bool {
		return len(sink.snapshot()) == 2
	}, 5*time.Second, 20*time.Millisecond, "parent cancel must produce request + partial-response rows")

	recs := sink.snapshot()
	assert.Equal(t, rawKindResponse, recs[1].Kind, "local cancellation stays a partial response row, not an error row")
	assert.Equal(t, 0, recs[1].HTTPStatus)
	for _, r := range recs {
		assert.NotEqual(t, rawKindError, r.Kind)
	}
}
