package agent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
)

// This file defines the raw-capture (llm-raw-capture capability) contract the
// OpenAI client consumes: the context values that carry capture metadata down
// to StreamChat, the record shape produced per capture, and the sink interface
// the production wiring implements (Redis write-behind buffer — see
// internal/llmraw). OpenAIClient holds the sink as an optional dependency so
// the client itself stays free of any store import.

// sessionCtxKey is the unexported context key type for the session_id value.
type sessionCtxKey struct{}

// agentCtxKey is the unexported context key type for the agent-name value.
type agentCtxKey struct{}

// WithSessionID returns a copy of ctx carrying sessionID so raw capture (and
// any future per-session consumer) can attribute an LLM call to its session
// without threading the id through the agent APIs. MessageStreamHandler
// injects it once per request; TitleService injects it into its background
// context. Empty sessionID returns ctx unchanged (matching trace.WithContext).
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// SessionIDFromContext returns the session_id stored in ctx, or an empty
// string when none is present.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionCtxKey{}).(string)
	return v
}

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
// rows of a call (request, per-frame chunks, response/error) share CallID and
// Seq; FrameIndex orders them within the call (request=0, chunks 1..N in
// arrival order, response/error last). Seq comes from the process-wide
// monotonic counter below so calls order stably within (and across) traces.
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
	rawKindChunk    = model.RawKindChunk
	rawKindResponse = model.RawKindResponse
	rawKindError    = model.RawKindError
)

// frameCaptureCap bounds the cumulative kind=chunk bytes captured per LLM
// call. Frame data is 50-100x the aggregated content (each SSE frame carries
// ~200B of envelope for a few tokens), so without a cap a runaway stream
// could overflow the MEDIUMTEXT row ceiling; past the cap a
// {"_truncated":true,...} marker row is emitted and further frames are
// skipped for that call (the stitched response row is unaffected).
const frameCaptureCap = 8 << 20

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
