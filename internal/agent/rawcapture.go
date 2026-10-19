package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/reqctx"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
)

// captureTextPart is the text content part inside a redacted user message.
type captureTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// captureRedactedPart is the structured stand-in for one image_url content
// part: the base64 payload is replaced by mime / byte length / sha256 /
// attachment_id. Field-level redaction, not truncation — the raw log's
// "parses to the as-sent structure" and "never truncate" invariants both
// survive, and the 16MiB MEDIUMTEXT ceiling stays safe no matter how many
// images a request carries.
type captureRedactedPart struct {
	Type         string `json:"type"`
	MIME         string `json:"mime"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

// captureUserMessage is the re-rendered form of an image-bearing user
// message: text part first, then one redacted part per image.
type captureUserMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// captureRequestJSON renders the raw-capture request row. Image-bearing
// user messages are re-rendered at BUILD time with their image_url parts
// already redacted, so the multi-megabyte base64 payloads never enter the
// capture document — nor any decode/re-encode intermediate. This keeps the
// capture path's peak memory proportional to the (tiny) redacted document
// even when several maximum-size images ride one request concurrently.
// sha256 is computed over the original image bytes directly (no base64
// round-trip). The vision side channel's injected image (req.vision)
// redacts the same way, without an attachment id.
//
// Assembly: the wire params are marshaled with Messages cleared (a small,
// image-free document holding every other as-sent field through the SDK's
// own marshaler — the params type has a custom MarshalJSON, so embedding it
// in a shadow struct would silently bypass any field override), then the
// per-message renderings are spliced in as the "messages" value.
func captureRequestJSON(req LLMRequest, params openai.ChatCompletionNewParams) ([]byte, error) {
	hasImages := req.vision != nil
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			hasImages = true
			break
		}
	}
	if !hasImages {
		return json.Marshal(params)
	}

	// The vision side channel attached its image to the LAST user message
	// in params (withVisionImage appends one when no user message exists).
	visionTarget := -1
	if req.vision != nil {
		for i := len(params.Messages) - 1; i >= 0; i-- {
			if params.Messages[i].OfUser != nil {
				visionTarget = i
				break
			}
		}
	}

	msgs := make([]json.RawMessage, len(params.Messages))
	for i := range params.Messages {
		var imgs []MessageImage
		if i < len(req.Messages) {
			imgs = req.Messages[i].Images
		}
		if len(imgs) == 0 && i != visionTarget {
			raw, err := json.Marshal(params.Messages[i])
			if err != nil {
				return nil, fmt.Errorf("raw capture: marshal message %d: %w", i, err)
			}
			msgs[i] = raw
			continue
		}
		raw, err := json.Marshal(redactedUserMessage(req, params, i, imgs, i == visionTarget))
		if err != nil {
			return nil, fmt.Errorf("raw capture: render redacted message %d: %w", i, err)
		}
		msgs[i] = raw
	}
	msgsJSON, err := json.Marshal(msgs)
	if err != nil {
		return nil, fmt.Errorf("raw capture: marshal redacted messages: %w", err)
	}

	sansMessages := params
	sansMessages.Messages = nil
	base, err := json.Marshal(sansMessages)
	if err != nil {
		return nil, fmt.Errorf("raw capture: marshal message-less params: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("raw capture: decode message-less params: %w", err)
	}
	doc["messages"] = msgsJSON
	return json.Marshal(doc)
}

// redactedUserMessage renders the image-bearing user message at index i:
// its text part (from the wire rendering when available, else the request's
// message content) followed by one redacted part per image. The vision side
// channel contributes its own image when includeVision is set.
func redactedUserMessage(req LLMRequest, params openai.ChatCompletionNewParams, i int, imgs []MessageImage, includeVision bool) captureUserMessage {
	text := ""
	if i < len(req.Messages) {
		text = req.Messages[i].Content
	}
	if u := params.Messages[i].OfUser; u != nil && u.Content.OfString.Valid() {
		text = u.Content.OfString.Value
	}
	out := captureUserMessage{Role: "user"}
	if text != "" {
		out.Content = append(out.Content, captureTextPart{Type: "text", Text: text})
	}
	for _, img := range imgs {
		out.Content = append(out.Content, redactedImagePart(img.MIMEType, img.Data, img.AttachmentID))
	}
	if includeVision && req.vision != nil {
		out.Content = append(out.Content, redactedImagePart(req.vision.MIMEType, req.vision.Data, ""))
	}
	return out
}

// redactedImagePart builds the metadata stand-in for one image payload.
func redactedImagePart(mime string, data []byte, attachmentID string) captureRedactedPart {
	sum := sha256.Sum256(data)
	return captureRedactedPart{
		Type:         "image_url_redacted",
		MIME:         mime,
		Bytes:        len(data),
		SHA256:       fmt.Sprintf("%x", sum),
		AttachmentID: attachmentID,
	}
}

// This file defines the raw-capture (llm-raw-capture capability) contract the
// OpenAI client consumes: the context values that carry capture metadata down
// to StreamChat, the record shape produced per capture, and the sink interface
// the production wiring implements (Redis write-behind buffer — see
// internal/llmraw). OpenAIClient holds the sink as an optional dependency so
// the client itself stays free of any store import.
// Capture emits the request and the stitched terminal response/error; individual
// SSE chunks are not captured.

// agentCtxKey is the unexported context key type for the agent-name value.
type agentCtxKey struct{}

// WithSessionID / SessionIDFromContext delegate to the shared reqctx leaf
// package (tool-call-log-correlation capability): the session-id key now
// lives there so internal/tool and internal/pkg/logger can read it without
// importing this package. These aliases keep the historical agent-side API
// (and its injection points — MessageStreamHandler, TitleService, raw-capture
// attribution) unchanged; the values are the SAME reqctx key both sides read.
var (
	WithSessionID        = reqctx.WithSessionID
	SessionIDFromContext = reqctx.SessionIDFromContext
)

// WithUserID / UserIDFromContext alias the per-user identity key maintained
// by internal/tool/skill (the value Orchestrator.Handle injects for every
// turn). Aliasing it here keeps handler/service call sites that need to stamp
// a user onto a context (title generation's detached background context)
// within the agent package's existing import surface.
var (
	WithUserID        = skill.WithUserID
	UserIDFromContext = skill.UserIDFromContext
)

// WithAgentName returns a copy of ctx carrying the display name of the agent
// about to run, so raw capture can attribute the LLM rounds that follow —
// including the round-cap wrap-up round, which inherits the caller's ctx — to
// the right agent. Each agent's Run injects its own name at entry, so
// sub-agents dispatched by Confucius override the value for their own rounds.
func WithAgentName(ctx context.Context, agentName string) context.Context {
	if agentName == "" {
		return ctx
	}
	return context.WithValue(ctx, agentCtxKey{}, agentName)
}

// AgentNameFromContext returns the agent name stored in ctx, or an empty
// string when none is present.
func AgentNameFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(agentCtxKey{}).(string)
	return v
}

// RawCaptureSink receives raw LLM capture records. It is invoked on the
// streaming hot path — implementations MUST be non-blocking (or bounded by a
// short internal timeout) and MUST NOT fail the call: an error is logged
// internally and the record dropped, per the spec's write-behind
// best-effort requirement. A nil sink disables capture entirely.
type RawCaptureSink interface {
	Capture(ctx context.Context, rec RawCaptureRecord)
}

// RawCaptureRecord is one raw payload captured around a single LLM call. All
// rows of a call (request, response/error) share CallID and Seq; FrameIndex
// orders them within the call (request=0, response/error=1). Seq comes from
// the process-wide monotonic counter below so calls order stably within (and
// across) traces.
// Field set mirrors the llm_raw_log table columns.
type RawCaptureRecord struct {
	CallID       string    `json:"call_id"`
	Seq          int       `json:"seq"`
	FrameIndex   int       `json:"frame_index"`
	TraceID      string    `json:"trace_id"`
	SessionID    string    `json:"session_id"`
	UserID       string    `json:"user_id"`
	Agent        string    `json:"agent"`
	Kind         string    `json:"kind"`
	Model        string    `json:"model"`
	FinishReason string    `json:"finish_reason"`
	HTTPStatus   int       `json:"http_status"`
	DurationMS   int64     `json:"duration_ms"`
	Raw          string    `json:"raw"`
	MsgTime      time.Time `json:"msg_time"`
}

// rawCallSeq is the process-wide monotonic capture sequence. A per-trace map
// would order rows within a trace only; the global counter preserves the same
// relative order per trace (calls of one trace are issued by one process, in
// sequence) while avoiding unbounded map growth (design D4 risk note).
var rawCallSeq atomic.Int64

// Capture-row kind values (agent-local aliases of the storage-side constants
// so this package's capture contract stays self-contained against
// internal/model).
const (
	rawKindRequest  = model.RawKindRequest
	rawKindResponse = model.RawKindResponse
	rawKindError    = model.RawKindError
)

// newRawCall mints the (call_id, seq) pair shared by one LLM call's request
// and response/error rows.
func newRawCall() (callID string, seq int) {
	return uuid.NewString(), int(rawCallSeq.Add(1))
}

// captureMeta reads the attribution fields StreamChat decorates its records
// with: trace_id / session_id / agent from context (agent falls back to
// "unknown" so rows are never unattributable), user_id from the skill
// package's context value (injected by AuthMiddleware-driven wiring).
func captureMeta(ctx context.Context) (traceID, sessionID, userID, agentName string) {
	agentName = AgentNameFromContext(ctx)
	if agentName == "" {
		agentName = "unknown"
	}
	return trace.FromContext(ctx), SessionIDFromContext(ctx), skill.UserIDFromContext(ctx), agentName
}

// capture emits one record to the sink (no-op when no sink is attached or the
// call has no capture identity). StreamChat is the only caller.
func (c *OpenAIClient) capture(ctx context.Context, callID string, seq, frameIndex int, kind, modelName, finishReason string, httpStatus int, dur time.Duration, raw string) {
	if c.sink == nil || callID == "" {
		return
	}
	traceID, sessionID, userID, agentName := captureMeta(ctx)
	c.sink.Capture(ctx, RawCaptureRecord{
		CallID:       callID,
		Seq:          seq,
		FrameIndex:   frameIndex,
		TraceID:      traceID,
		SessionID:    sessionID,
		UserID:       userID,
		Agent:        agentName,
		Kind:         kind,
		Model:        modelName,
		FinishReason: finishReason,
		HTTPStatus:   httpStatus,
		DurationMS:   dur.Milliseconds(),
		Raw:          raw,
		MsgTime:      time.Now().UTC(),
	})
}

// rawCaptureCompletion renders the stitched, non-streaming-equivalent chat
// completion stored as the kind=response payload: streamed deltas reassembled
// into the single object a non-streaming call would have returned. Field
// names follow the OpenAI chat.completion schema so the payload reads like
// ground truth when debugging.
type rawCaptureCompletion struct {
	ID      string             `json:"id,omitempty"`
	Object  string             `json:"object"`
	Created int64              `json:"created,omitempty"`
	Model   string             `json:"model"`
	Choices []rawCaptureChoice `json:"choices"`
	Usage   *rawCaptureUsage   `json:"usage,omitempty"`
}

type rawCaptureChoice struct {
	Index        int               `json:"index"`
	Message      rawCaptureMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type rawCaptureMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type rawCaptureUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

// stitchedCompletion assembles the response payload from the aggregated
// stream state. id/created carry the first chunk's identity when the gateway
// supplied one. usage is omitted when the gateway never sent any (some
// OpenAI-compatible endpoints don't — the raw log makes that visible).
func stitchedCompletion(id string, created int64, modelName string, resp LLMResponse) rawCaptureCompletion {
	msg := rawCaptureMessage{
		Role:             "assistant",
		Content:          resp.Content,
		ReasoningContent: resp.ReasoningContent,
		ToolCalls:        resp.ToolCalls,
	}
	var usage *rawCaptureUsage
	if resp.Usage.TotalTokens > 0 {
		usage = &rawCaptureUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			ReasoningTokens:  resp.Usage.ReasoningTokens,
		}
	}
	return rawCaptureCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   modelName,
		Choices: []rawCaptureChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: resp.FinishReason,
		}},
		Usage: usage,
	}
}
