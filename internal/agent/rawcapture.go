package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/reqctx"
	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
)

// redactImageParts rewrites every image_url content part in a marshaled
// chat-completion request payload (add-image-attachments): the base64 data
// URL is replaced by a structured image_url_redacted object carrying
// mime / byte length / sha256 / attachment_id. The replacement is a
// field-level redaction, not a truncation — the raw log's "parses to the
// as-sent structure" and "never truncate" invariants both survive, and the
// 16MiB MEDIUMTEXT ceiling stays safe no matter how many images a request
// carries. attachment IDs are matched by content digest from the request's
// in-memory message images; the vision side channel (no attachment identity)
// redacts with an empty id.
func redactImageParts(raw []byte, msgs []Message) []byte {
	if !strings.Contains(string(raw), "image_url") {
		return raw
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	idsByDigest := map[string]string{}
	for _, m := range msgs {
		for _, img := range m.Images {
			sum := sha256.Sum256(img.Data)
			idsByDigest[fmt.Sprintf("%x", sum)] = img.AttachmentID
		}
	}
	changed := redactWalk(doc, idsByDigest)
	if !changed {
		return raw
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// redactWalk descends the decoded payload replacing every image part (an
// object with an image_url.url data URL) with its redacted twin. It reports
// whether any replacement happened.
func redactWalk(node any, idsByDigest map[string]string) bool {
	switch v := node.(type) {
	case map[string]any:
		if iu, ok := v["image_url"].(map[string]any); ok {
			if url, ok := iu["url"].(string); ok && strings.HasPrefix(url, "data:") {
				redactImagePart(v, url, idsByDigest)
				return true
			}
		}
		changed := false
		for k, child := range v {
			if redactWalk(child, idsByDigest) {
				changed = true
				_ = k
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range v {
			if redactWalk(child, idsByDigest) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// redactImagePart mutates part (in place) into the redacted form for one
// data-URL image part.
func redactImagePart(part map[string]any, dataURL string, idsByDigest map[string]string) {
	mime := ""
	payload := dataURL
	if idx := strings.Index(dataURL, ";base64,"); idx > 5 {
		mime = strings.TrimPrefix(dataURL[5:idx], "data:")
		payload = dataURL[idx+8:]
	}
	var digest string
	var byteLen int
	if raw, err := base64.StdEncoding.DecodeString(payload); err == nil {
		byteLen = len(raw)
		sum := sha256.Sum256(raw)
		digest = fmt.Sprintf("%x", sum)
	} else {
		byteLen = len(payload)
	}
	for k := range part {
		delete(part, k)
	}
	part["type"] = "image_url_redacted"
	part["mime"] = mime
	part["bytes"] = byteLen
	part["sha256"] = digest
	if id := idsByDigest[digest]; id != "" {
		part["attachment_id"] = id
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
