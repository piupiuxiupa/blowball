package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"go.uber.org/zap"
)

// previewLimit is the maximum number of characters written for content or
// argument previews in debug logs. Longer values are truncated with an ellipsis.
const previewLimit = 500

// truncatePreview returns s truncated to previewLimit runes, appending "…" when
// truncated. It is safe for empty strings and ASCII/Unicode content.
func truncatePreview(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= previewLimit {
		return s
	}
	return string(runes[:previewLimit]) + "…"
}

// toolNamePreviews extracts function names from the JSON-encoded OpenAI tools
// payload. It tolerates malformed JSON by skipping entries it cannot parse.
func toolNamePreviews(tools []byte) []string {
	type rawTool struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	var raws []rawTool
	if err := json.Unmarshal(tools, &raws); err != nil {
		return nil
	}
	names := make([]string, 0, len(raws))
	for _, r := range raws {
		if r.Type != "" && r.Type != "function" {
			continue
		}
		var fn struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(r.Function, &fn); err != nil {
			continue
		}
		if fn.Name != "" {
			names = append(names, fn.Name)
		}
	}
	return names
}

// logLLMRequest emits a structured debug entry summarizing the request sent to
// the underlying model. It excludes the API key and Authorization header.
func logLLMRequest(ctx context.Context, req LLMRequest) {
	traceID := trace.FromContext(ctx)
	logMessages := make([]map[string]string, len(req.Messages))
	for i, m := range req.Messages {
		logMessages[i] = map[string]string{
			"role":            m.Role,
			"content_preview": truncatePreview(m.Content),
		}
	}

	fields := []zap.Field{
		zap.String("event", "llm_request"),
		zap.String("model", req.Model),
		zap.Int("message_count", len(req.Messages)),
		zap.Any("messages", logMessages),
	}
	if traceID != "" {
		fields = append(fields, zap.String("trace_id", traceID))
	}
	if req.MaxTokens > 0 {
		fields = append(fields, zap.Int("max_tokens", req.MaxTokens))
	}
	if req.Thinking {
		fields = append(fields, zap.Bool("thinking", true))
		fields = append(fields, zap.String("reasoning_effort", req.ReasoningEffort))
	}
	if len(req.Tools) > 0 {
		names := toolNamePreviews(req.Tools)
		fields = append(fields, zap.Int("tools_count", len(names)))
		fields = append(fields, zap.Any("tools_preview", names))
	}
	logger.L().Debug("LLM request", fields...)
}

// logLLMResponse emits a structured debug entry summarizing the aggregated
// response returned by the underlying model.
func logLLMResponse(ctx context.Context, resp LLMResponse) {
	traceID := trace.FromContext(ctx)
	toolCalls := make([]map[string]string, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		toolCalls[i] = map[string]string{
			"name":              tc.Function.Name,
			"arguments_preview": truncatePreview(tc.Function.Arguments),
		}
	}

	fields := []zap.Field{
		zap.String("event", "llm_response"),
		zap.String("finish_reason", resp.FinishReason),
		zap.Int("content_len", len([]rune(resp.Content))),
		zap.String("content_preview", truncatePreview(resp.Content)),
		zap.Any("tool_calls", toolCalls),
		zap.Int("prompt_tokens", resp.Usage.PromptTokens),
		zap.Int("completion_tokens", resp.Usage.CompletionTokens),
		zap.Int("total_tokens", resp.Usage.TotalTokens),
		zap.Int("reasoning_tokens", resp.Usage.ReasoningTokens),
	}
	if resp.ReasoningContent != "" {
		fields = append(fields, zap.Int("reasoning_content_len", len([]rune(resp.ReasoningContent))))
		fields = append(fields, zap.String("reasoning_content_preview", truncatePreview(resp.ReasoningContent)))
	}
	if traceID != "" {
		fields = append(fields, zap.String("trace_id", traceID))
	}
	logger.L().Debug("LLM response", fields...)
}

// ErrStreamIdleTimeout is returned by StreamChat when the stream idle
// watchdog aborted a call: no SSE frame arrived within the configured
// openai.stream_idle_timeout window (the gap starts at call issuance, so it
// also covers a first frame that never comes). The wrapping error's message
// carries "timeout" so the existing substring classifier (isTransientError,
// retry.go) treats it as retryable with zero retry-logic changes.
var ErrStreamIdleTimeout = errors.New("openai client: stream idle timeout")

// OpenAIClient is the production LLMClient backed by openai-go v3. It is the
// only file in this package that imports openai-go, so swapping SDKs (or
// pointing at a non-OpenAI compatible endpoint) only touches this file.
type OpenAIClient struct {
	client openai.Client
	// sink optionally receives raw request/response/error captures for the
	// llm-raw-capture capability. Nil disables capture entirely; see
	// rawcapture.go.
	sink RawCaptureSink
	// streamIdleTimeout bounds the maximum gap between consecutive SSE frames
	// in StreamChat (llm-stream-watchdog capability). Zero disables the
	// watchdog — byte-for-byte prior behavior.
	streamIdleTimeout time.Duration
}

// NewOpenAIClient builds an OpenAIClient from the OpenAI section of config.
// baseURL is optional; when empty the SDK default is used. Raw capture is
// disabled; use NewOpenAIClientWithSink to enable it.
func NewOpenAIClient(cfg config.OpenAIConfig) *OpenAIClient {
	return NewOpenAIClientWithSink(cfg, nil)
}

// NewOpenAIClientWithSink is NewOpenAIClient with a raw-capture sink attached.
// The sink receives the as-sent request params when a call starts and the
// stitched response (or gateway error body) when it ends; see rawcapture.go.
func NewOpenAIClientWithSink(cfg config.OpenAIConfig, sink RawCaptureSink) *OpenAIClient {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &OpenAIClient{client: openai.NewClient(opts...), sink: sink, streamIdleTimeout: cfg.StreamIdleTimeout}
}

// NewOpenAIClientFromClient wires an externally-constructed openai.Client —
// used by tests / Phase 10 bootstrap paths that want to share one client. The
// streamIdleTimeout parameter arms the idle watchdog the same way
// NewOpenAIClientWithSink would from config (0 disables it).
func NewOpenAIClientFromClient(c openai.Client, streamIdleTimeout time.Duration) *OpenAIClient {
	return &OpenAIClient{client: c, streamIdleTimeout: streamIdleTimeout}
}

// StreamChat implements LLMClient. It opens a streaming chat completion,
// drains the SSE chunk stream, calls onToken for each content delta, and
// aggregates the final assistant content, tool_calls, finish_reason, and
// usage. Tool-call fragments arrive incrementally across chunks (id, name,
// and arguments split into many deltas); they are stitched back together
// here keyed by the chunk's tool-call Index.
func (c *OpenAIClient) StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error, onReasoning func(string) error) (LLMResponse, error) {
	if c == nil {
		return LLMResponse{}, fmt.Errorf("openai client: nil receiver")
	}

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: toOpenAIMessages(req.Messages),
	}
	// B2 wire family (model-effort-v2): the shape follows the resolved
	// catalog entry's thinking capability. A thinking entry ALWAYS carries
	// reasoning_effort — the literal "none" included, so OpenAI-compatible
	// gateways actually disable thinking instead of falling back to the
	// model's own default level — and maps the max_tokens quota to
	// max_completion_tokens. A non-thinking entry never sends
	// reasoning_effort and uses plain max_tokens. Sampling parameters
	// (temperature etc.) are sent on neither family (the former always-0
	// temperature is dead code, removed).
	if req.Thinking {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
		if req.MaxTokens > 0 {
			params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
		}
	} else {
		if req.MaxTokens > 0 {
			params.MaxTokens = openai.Int(int64(req.MaxTokens))
		}
	}
	if len(req.Tools) > 0 {
		tools, err := parseOpenAITools(req.Tools)
		if err != nil {
			return LLMResponse{}, fmt.Errorf("openai client: parse tools: %w", err)
		}
		params.Tools = tools
	}
	if len(req.ResponseFormat) > 0 {
		rf, err := parseResponseFormat(req.ResponseFormat)
		if err != nil {
			return LLMResponse{}, fmt.Errorf("openai client: parse response_format: %w", err)
		}
		params.ResponseFormat = rf
	}

	logLLMRequest(ctx, req)

	// Raw capture (llm-raw-capture capability): mint the call identity and
	// emit the request row — the exact params as sent — before the stream
	// starts, so a call that dies mid-stream still leaves its request side.
	// frameIdx tracks the last emitted record ordinal within the call
	// (request=0, per-frame chunks 1..N, response/error last); frameBytes and
	// framesCapped implement the per-call frame-capture budget.
	var callID string
	var seq int
	var start time.Time
	var respID string
	var respCreated int64
	var frameIdx int
	var frameBytes int
	var framesCapped bool
	start = time.Now()
	if c.sink != nil {
		callID, seq = newRawCall()
		if raw, err := json.Marshal(params); err != nil {
			logger.L().Warn("raw capture: marshal request params failed; skipping request row", zap.Error(err))
		} else {
			c.capture(ctx, callID, seq, 0, rawKindRequest, req.Model, "", 0, 0, string(raw))
		}
	}

	// Stream idle watchdog (llm-stream-watchdog capability): bound the
	// maximum gap between consecutive SSE frames. The timer starts BEFORE
	// NewStreaming, so the first gap covers time-to-first-frame; the read
	// loop kicks the watchdog after every accepted frame (any frame — empty
	// delta, usage-only, reasoning — proves the stream is alive). When no
	// frame arrives within the window the timer fires and cancels callCtx,
	// unblocking stream.Next() so the call returns a typed error instead of
	// hanging the turn forever. The goroutine cannot leak: it selects on
	// callCtx.Done() and cancel is deferred below. idle <= 0 leaves callCtx
	// as the parent ctx — byte-for-byte prior behavior, no goroutine.
	callCtx := ctx
	var watchdogReset chan struct{} // non-nil only while the watchdog is armed
	var watchdogFired atomic.Bool
	if idle := c.streamIdleTimeout; idle > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		watchdogReset = make(chan struct{}, 1)
		go func() {
			timer := time.NewTimer(idle)
			defer timer.Stop()
			for {
				select {
				case <-timer.C:
					watchdogFired.Store(true)
					cancel() // unblocks stream.Next()
					return
				case <-watchdogReset:
					// Single-owner drain-and-reset: stop the timer and
					// clear a pending fire so a kick racing the deadline
					// cannot masquerade as the next idle gap.
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(idle)
				case <-callCtx.Done():
					return // normal end or parent cancel — always exits
				}
			}
		}()
	}
	// kickWatchdog resets the idle gap after an accepted frame. The buffered-1
	// channel coalesces bursts, so the non-blocking send never slows the read
	// loop and a kick racing the timer fire resolves next loop iteration.
	kickWatchdog := func() {
		if watchdogReset == nil {
			return
		}
		select {
		case watchdogReset <- struct{}{}:
		default:
		}
	}

	stream := c.client.Chat.Completions.NewStreaming(callCtx, params)
	defer stream.Close()

	var (
		resp             LLMResponse
		finish           string
		toolStitch       = newToolCallStitcher()
		reasoningContent strings.Builder
		// frames counts accepted SSE frames (every loop iteration), independent
		// of the capture-side frameIdx: the idle-timeout diagnostics report
		// what the wire delivered even when no sink is attached or capture was
		// budget-capped.
		frames int
	)

	// capturePartial emits a response row for a stream that ended without the
	// gateway failing it (context cancellation, hub close): what was streamed
	// so far, with the raw finish_reason (no forced "stop"), and without
	// mutating the resp the caller sees. Per design D6 a local cancellation is
	// not an error row.
	capturePartial := func() {
		if c.sink == nil || callID == "" {
			return
		}
		p := resp
		p.FinishReason = finish
		p.ToolCalls = toolStitch.finalize()
		p.ReasoningContent = reasoningContent.String()
		if raw, err := json.Marshal(stitchedCompletion(respID, respCreated, req.Model, p)); err == nil {
			c.capture(ctx, callID, seq, frameIdx+1, rawKindResponse, req.Model, p.FinishReason, 0, time.Since(start), string(raw))
		}
	}
	// captureError emits the error row: gateway HTTP status plus the raw
	// error body when openai-go exposes one (apierror), else the error string.
	captureError := func(err error) {
		if c.sink == nil || callID == "" {
			return
		}
		httpStatus := 0
		rawBody := err.Error()
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			httpStatus = apiErr.StatusCode
			if body := apiErr.RawJSON(); body != "" {
				rawBody = body
			}
		}
		c.capture(ctx, callID, seq, frameIdx+1, rawKindError, req.Model, finish, httpStatus, time.Since(start), rawBody)
	}
	// captureFrame emits one kind=chunk row for the chunk just received, with
	// its verbatim wire bytes, and enforces the per-call frame budget: past
	// frameCaptureCap it emits an explicit truncation marker row and stops
	// capturing frames for the rest of the call.
	captureFrame := func(chunk openai.ChatCompletionChunk) {
		if c.sink == nil || callID == "" || framesCapped {
			return
		}
		frameJSON := chunk.RawJSON()
		if frameJSON == "" {
			// No wire bytes on the chunk (synthetic/decoded-only path);
			// re-marshal so the frame is still captured, just not byte-exact.
			if b, err := json.Marshal(chunk); err == nil {
				frameJSON = string(b)
			}
		}
		if frameJSON == "" {
			return
		}
		frameIdx++
		frameBytes += len(frameJSON)
		c.capture(ctx, callID, seq, frameIdx, rawKindChunk, req.Model, "", 0, 0, frameJSON)
		if frameBytes < frameCaptureCap {
			return
		}
		framesCapped = true
		if raw, err := json.Marshal(map[string]any{
			"_truncated": true,
			"reason":     "frame_capture_cap",
			"frames":     frameIdx,
			"bytes":      frameBytes,
		}); err == nil {
			frameIdx++
			c.capture(ctx, callID, seq, frameIdx, rawKindChunk, req.Model, "", 0, 0, string(raw))
		}
		logger.L().Warn("raw capture: frame budget reached; further chunks not captured for this call",
			zap.String("call_id", callID),
			zap.Int("frames", frameIdx),
			zap.Int("bytes", frameBytes))
	}

	for stream.Next() {
		if err := ctx.Err(); err != nil {
			capturePartial()
			return resp, err
		}
		frames++
		kickWatchdog()
		chunk := stream.Current()
		if respID == "" && chunk.ID != "" {
			respID, respCreated = chunk.ID, chunk.Created
		}
		captureFrame(chunk)
		if chunk.Usage.TotalTokens > 0 {
			resp.Usage = Usage{
				PromptTokens:     int(chunk.Usage.PromptTokens),
				CompletionTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:      int(chunk.Usage.TotalTokens),
			}
			if chunk.Usage.JSON.CompletionTokensDetails.Valid() {
				resp.Usage.ReasoningTokens = int(chunk.Usage.CompletionTokensDetails.ReasoningTokens)
			}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
			delta := choice.Delta
			if delta.Content != "" {
				resp.Content += delta.Content
				if onToken != nil {
					if err := onToken(delta.Content); err != nil {
						capturePartial()
						return resp, err
					}
				}
			}
			if rcField, ok := delta.JSON.ExtraFields["reasoning_content"]; ok {
				raw := rcField.Raw()
				if raw != "" && raw != "null" {
					var rc string
					if err := json.Unmarshal([]byte(raw), &rc); err == nil {
						reasoningContent.WriteString(rc)
						if onReasoning != nil {
							if err := onReasoning(rc); err != nil {
								capturePartial()
								return resp, err
							}
						}
					}
				}
			}
			toolStitch.ingest(delta.ToolCalls)
		}
	}
	if err := stream.Err(); err != nil && err != io.EOF {
		// ssestream surfaces context cancellation as an error; surface ctx.Err
		// directly when applicable so callers can branch on cancellation.
		// The parent check comes FIRST (design D3): a canceled request cannot
		// be retried anyway, so parent cancellation wins any race with the
		// watchdog timer firing concurrently.
		if ctxErr := ctx.Err(); ctxErr != nil {
			capturePartial()
			return resp, ctxErr
		}
		// The watchdog fired — the stream went silent past the configured
		// idle window. Abort with the typed error (whose "timeout" message
		// the transient classifier picks up) instead of surfacing the
		// cancel-induced read error, and leave an error row for post-mortem
		// (distinct from the partial-response row a local cancellation gets).
		if watchdogFired.Load() {
			idleErr := fmt.Errorf("%w: no frame for %s (frames=%d, model=%s)",
				ErrStreamIdleTimeout, c.streamIdleTimeout, frames, req.Model)
			logger.L().Warn("LLM stream idle timeout: stalled stream aborted",
				zap.String("event", "llm_stream_idle_timeout"),
				zap.String("agent", AgentNameFromContext(ctx)),
				zap.String("model", req.Model),
				zap.Int("frames", frames),
				zap.Duration("idle", c.streamIdleTimeout),
				zap.Duration("elapsed", time.Since(start)))
			captureError(idleErr)
			return resp, idleErr
		}
		captureError(err)
		return resp, fmt.Errorf("openai client: stream: %w", err)
	}

	resp.FinishReason = finish
	if resp.FinishReason == "" {
		resp.FinishReason = "stop"
	}
	resp.ToolCalls = toolStitch.finalize()
	resp.ReasoningContent = reasoningContent.String()

	if c.sink != nil && callID != "" {
		if raw, err := json.Marshal(stitchedCompletion(respID, respCreated, req.Model, resp)); err == nil {
			c.capture(ctx, callID, seq, frameIdx+1, rawKindResponse, req.Model, resp.FinishReason, 0, time.Since(start), string(raw))
		}
	}

	logLLMResponse(ctx, resp)
	return resp, nil
}

// toOpenAIMessages converts our Message slice into the openai-go union
// message param shape. System / user / assistant / tool are all supported.
func toOpenAIMessages(msgs []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toOpenAIMessage(m))
	}
	return out
}

func toOpenAIMessage(m Message) openai.ChatCompletionMessageParamUnion {
	switch m.Role {
	case "system", "developer":
		return openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{
					OfString: openai.String(m.Content),
				},
			},
		}
	case "user":
		return openai.ChatCompletionMessageParamUnion{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{
					OfString: openai.String(m.Content),
				},
			},
		}
	case "tool":
		return openai.ChatCompletionMessageParamUnion{
			OfTool: &openai.ChatCompletionToolMessageParam{
				Content: openai.ChatCompletionToolMessageParamContentUnion{
					OfString: openai.String(m.Content),
				},
				ToolCallID: m.ToolCallID,
			},
		}
	case "assistant":
		a := &openai.ChatCompletionAssistantMessageParam{
			Content: openai.ChatCompletionAssistantMessageParamContentUnion{
				OfString: openai.String(m.Content),
			},
		}
		if len(m.ToolCalls) > 0 {
			a.ToolCalls = toOpenAIToolCalls(m.ToolCalls)
		}
		if m.ReasoningContent != "" {
			a.SetExtraFields(map[string]any{"reasoning_content": m.ReasoningContent})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: a}
	}
	// Unknown role — treat as user to avoid silent drops in production.
	return openai.ChatCompletionMessageParamUnion{
		OfUser: &openai.ChatCompletionUserMessageParam{
			Content: openai.ChatCompletionUserMessageParamContentUnion{
				OfString: openai.String(m.Content),
			},
		},
	}
}

func toOpenAIToolCalls(calls []ToolCall) []openai.ChatCompletionMessageToolCallUnionParam {
	out := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(calls))
	for _, c := range calls {
		out = append(out, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: c.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Arguments: c.Function.Arguments,
					Name:      c.Function.Name,
				},
			},
		})
	}
	return out
}

// parseOpenAITools unmarshals a JSON tools[] payload (as produced by
// tool.Registry.OpenAITools) into the openai-go union tool param slice.
func parseOpenAITools(b []byte) ([]openai.ChatCompletionToolUnionParam, error) {
	type rawTool struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	var raws []rawTool
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, err
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(raws))
	for _, r := range raws {
		var fd shared.FunctionDefinitionParam
		if err := json.Unmarshal(r.Function, &fd); err != nil {
			return nil, fmt.Errorf("unmarshal function definition: %w", err)
		}
		out = append(out, openai.ChatCompletionFunctionTool(fd))
	}
	return out, nil
}

// parseResponseFormat converts a raw JSON response_format payload (as set by
// a sub-agent on its final round) into the openai-go union param. The wire
// shape is the full OpenAI response_format object:
//
//	{"type":"json_schema","json_schema":{"name":"...","schema":{...},"strict":true}}
//
// Only the json_schema variant is supported (the structured-output shape used
// by capability A); an unknown type is rejected so a malformed schema fails
// fast at the LLM call rather than silently producing free text.
func parseResponseFormat(raw json.RawMessage) (openai.ChatCompletionNewParamsResponseFormatUnion, error) {
	var wire struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return openai.ChatCompletionNewParamsResponseFormatUnion{}, err
	}
	switch wire.Type {
	case "json_schema":
		var inner shared.ResponseFormatJSONSchemaJSONSchemaParam
		if err := json.Unmarshal(wire.JSONSchema, &inner); err != nil {
			return openai.ChatCompletionNewParamsResponseFormatUnion{}, fmt.Errorf("unmarshal json_schema: %w", err)
		}
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: inner},
		}, nil
	case "":
		return openai.ChatCompletionNewParamsResponseFormatUnion{}, fmt.Errorf("response_format: missing type")
	}
	return openai.ChatCompletionNewParamsResponseFormatUnion{}, fmt.Errorf("response_format: unsupported type %q", wire.Type)
}

// toolCallStitcher accumulates streamed tool-call deltas keyed by Index. The
// OpenAI streaming protocol splits a single tool_call across many chunks: the
// first carries the id and function.name, subsequent chunks append fragments
// to function.arguments. Stitching by Index is required because chunks do not
// repeat the name/id once emitted.
type toolCallStitcher struct {
	byIndex map[int64]*ToolCall
	order   []int64
}

func newToolCallStitcher() *toolCallStitcher {
	return &toolCallStitcher{byIndex: make(map[int64]*ToolCall)}
}

func (s *toolCallStitcher) ingest(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) {
	for _, d := range deltas {
		tc, ok := s.byIndex[d.Index]
		if !ok {
			tc = &ToolCall{}
			s.byIndex[d.Index] = tc
			s.order = append(s.order, d.Index)
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Function.Name != "" {
			tc.Function.Name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			tc.Function.Arguments += d.Function.Arguments
		}
	}
}

func (s *toolCallStitcher) finalize() []ToolCall {
	if len(s.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(s.order))
	for _, idx := range s.order {
		out = append(out, *s.byIndex[idx])
	}
	return out
}
