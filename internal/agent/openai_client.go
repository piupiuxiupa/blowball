package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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

// OpenAIClient is the production LLMClient backed by openai-go v3. It is the
// only file in this package that imports openai-go, so swapping SDKs (or
// pointing at a non-OpenAI compatible endpoint) only touches this file.
type OpenAIClient struct {
	client openai.Client
}

// NewOpenAIClient builds an OpenAIClient from the OpenAI section of config.
// baseURL is optional; when empty the SDK default is used.
func NewOpenAIClient(cfg config.OpenAIConfig) *OpenAIClient {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &OpenAIClient{client: openai.NewClient(opts...)}
}

// NewOpenAIClientFromClient wires an externally-constructed openai.Client —
// used by tests / Phase 10 bootstrap paths that want to share one client.
func NewOpenAIClientFromClient(c openai.Client) *OpenAIClient {
	return &OpenAIClient{client: c}
}

// StreamChat implements LLMClient. It opens a streaming chat completion,
// drains the SSE chunk stream, calls onToken for each content delta, and
// aggregates the final assistant content, tool_calls, finish_reason, and
// usage. Tool-call fragments arrive incrementally across chunks (id, name,
// and arguments split into many deltas); they are stitched back together
// here keyed by the chunk's tool-call Index.
func (c *OpenAIClient) StreamChat(ctx context.Context, req LLMRequest, onToken func(string) error) (LLMResponse, error) {
	if c == nil {
		return LLMResponse{}, fmt.Errorf("openai client: nil receiver")
	}

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: toOpenAIMessages(req.Messages),
	}
	if req.Thinking {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
		if req.MaxTokens > 0 {
			params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
		}
	} else {
		if req.MaxTokens > 0 {
			params.MaxTokens = openai.Int(int64(req.MaxTokens))
		}
		if req.Temperature != 0 {
			params.Temperature = openai.Float(float64(req.Temperature))
		}
	}
	if len(req.Tools) > 0 {
		tools, err := parseOpenAITools(req.Tools)
		if err != nil {
			return LLMResponse{}, fmt.Errorf("openai client: parse tools: %w", err)
		}
		params.Tools = tools
	}

	logLLMRequest(ctx, req)

	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		resp             LLMResponse
		finish           string
		toolStitch       = newToolCallStitcher()
		reasoningContent strings.Builder
	)
	for stream.Next() {
		if err := ctx.Err(); err != nil {
			return resp, err
		}
		chunk := stream.Current()
		if chunk.Usage.TotalTokens > 0 {
			resp.Usage = Usage{
				PromptTokens:     int(chunk.Usage.PromptTokens),
				CompletionTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:      int(chunk.Usage.TotalTokens),
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
					}
				}
			}
			toolStitch.ingest(delta.ToolCalls)
		}
	}
	if err := stream.Err(); err != nil && err != io.EOF {
		// ssestream surfaces context cancellation as an error; surface ctx.Err
		// directly when applicable so callers can branch on cancellation.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return resp, ctxErr
		}
		return resp, fmt.Errorf("openai client: stream: %w", err)
	}

	resp.FinishReason = finish
	if resp.FinishReason == "" {
		resp.FinishReason = "stop"
	}
	resp.ToolCalls = toolStitch.finalize()
	resp.ReasoningContent = reasoningContent.String()

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
