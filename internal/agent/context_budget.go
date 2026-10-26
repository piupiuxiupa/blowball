package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lush/blowball/internal/tokens"
)

type requestEnvelopeContextKey struct{}
type forcedCompactionContextKey struct{}
type liveMessagesContextKey struct{}

// ForcedCompaction carries an explicit force signal to compaction hooks.
// Provider usage is not used as a control signal: gateways may legitimately
// report zero, or omit usage entirely.
type ForcedCompaction struct {
	Reason          string
	EstimatedTokens int
	InputBudget     int
}

func WithForcedCompactionEstimate(ctx context.Context, reason string, estimatedTokens, inputBudget int) context.Context {
	return context.WithValue(ctx, forcedCompactionContextKey{}, ForcedCompaction{
		Reason:          reason,
		EstimatedTokens: estimatedTokens,
		InputBudget:     inputBudget,
	})
}

func ForcedCompactionFromContext(ctx context.Context) (ForcedCompaction, bool) {
	if ctx == nil {
		return ForcedCompaction{}, false
	}
	force, ok := ctx.Value(forcedCompactionContextKey{}).(ForcedCompaction)
	return force, ok
}

// RequestEnvelope is the fixed part of the outgoing model request that is not
// represented in conversation messages: system prompt and serialized tools.
type RequestEnvelope struct {
	System string
	Tools  []byte
}

// WithRequestEnvelope carries the exact system/tool envelope into compaction
// hooks so candidate budgeting can measure the same request the agent will
// send.
func WithRequestEnvelope(ctx context.Context, system string, tools []byte) context.Context {
	return context.WithValue(ctx, requestEnvelopeContextKey{}, RequestEnvelope{System: system, Tools: tools})
}

// RequestEnvelopeFromContext returns the installed request envelope, if any.
func RequestEnvelopeFromContext(ctx context.Context) (RequestEnvelope, bool) {
	if ctx == nil {
		return RequestEnvelope{}, false
	}
	env, ok := ctx.Value(requestEnvelopeContextKey{}).(RequestEnvelope)
	return env, ok
}

// WithLiveMessages carries the bounded replay projection of current-turn
// messages whose live-only fields cannot be reconstructed from persistence.
func WithLiveMessages(ctx context.Context, live []Message) context.Context {
	return context.WithValue(ctx, liveMessagesContextKey{}, append([]Message(nil), live...))
}

func LiveMessagesFromContext(ctx context.Context) []Message {
	if ctx == nil {
		return nil
	}
	live, _ := ctx.Value(liveMessagesContextKey{}).([]Message)
	return append([]Message(nil), live...)
}

const (
	// contextSafetyMarginTokens reserves room for provider-side framing and
	// estimator drift. It is deliberately fixed for the first rollout: the
	// completion quota is already model-specific, while this margin protects
	// every gateway from the same class of hard-window error.
	contextSafetyMarginTokens = 4096

	// jsonToolEnvelopeDensityDivisor is more conservative than the generic
	// four-characters-per-token estimate. Dense JSON (tool arguments, escaped
	// strings, ids) commonly tokenizes closer to 2.5-3 characters per token.
	jsonToolEnvelopeDensityDivisor = 3
)

// RequestBudget is the amount of context available for an LLM request's input.
// The output quota is reserved because gateways validate prompt plus requested
// completion against the model's hard window.
type RequestBudget struct {
	ModelWindow       int
	CompletionReserve int
	SafetyMargin      int
	InputTokens       int
}

// NewRequestBudget derives the request input budget. A non-positive window
// leaves the budget disabled (InputTokens 0), matching the existing
// compaction-disabled behavior. A positive window whose reserves consume the
// whole budget yields a negative InputTokens value so callers can reject the
// request explicitly.
func NewRequestBudget(modelWindow, completionReserve int) RequestBudget {
	b := RequestBudget{
		ModelWindow:       modelWindow,
		CompletionReserve: completionReserve,
		SafetyMargin:      contextSafetyMarginTokens,
	}
	if modelWindow <= 0 {
		return b
	}
	b.InputTokens = modelWindow - completionReserve - contextSafetyMarginTokens
	return b
}

// Valid reports a positive input budget. Disabled budgets are not "valid" for
// the purpose of a budgeted request, so callers must decide when disabled means
// bypass versus rejection.
func (b RequestBudget) Valid() bool { return b.InputTokens > 0 }

// ContextRequestEstimate is a conservative projection of the next request's
// input size. MeasuredUsage=true means it extends a provider-observed round;
// false means it is a complete rendered-request fallback.
type ContextRequestEstimate struct {
	Tokens        int
	MeasuredUsage bool
}

// EstimateRequestFromUsage extends a provider-observed prompt+completion size
// with the newly appended messages and protocol overhead. The caller must pass
// only messages appended after that measurement.
func EstimateRequestFromUsage(promptTokens, completionTokens int, appended []Message) ContextRequestEstimate {
	estimate := promptTokens + completionTokens + estimateMessages(appended)
	return ContextRequestEstimate{Tokens: estimate, MeasuredUsage: true}
}

// EstimateRenderedRequest computes the whole-request fallback from the final
// messages, system prompt, and serialized tools. system is separate because
// callers commonly rebuild it at request time.
func EstimateRenderedRequest(system string, msgs []Message, tools []byte) ContextRequestEstimate {
	all := make([]Message, 0, len(msgs)+1)
	if system != "" {
		all = append(all, Message{Role: "system", Content: system})
	}
	all = append(all, msgs...)
	tokensEstimate := estimateMessages(all)
	if len(tools) > 0 {
		tokensEstimate += estimateToolDescriptor(tools)
	}
	return ContextRequestEstimate{Tokens: tokensEstimate, MeasuredUsage: false}
}

// EstimateRequestOverBudget reports whether a conservative complete rendering
// of the next request would exceed budget. It is used as a preflight gate in
// the agent loop after a round's tool calls/results are appended.
func EstimateRequestOverBudget(system string, msgs []Message, tools []byte, budget int) bool {
	if budget <= 0 {
		return false
	}
	return EstimateRenderedRequest(system, msgs, tools).Tokens > budget
}

// estimateMessages is deliberately a little conservative for structured
// content: message bodies use the shared CJK-aware estimator and JSON/tool
// envelopes get a denser multiplier. Role/framing overhead is counted
// separately so a large number of tiny messages does not disappear.
func estimateMessages(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += 8 // message envelope / role / protocol markers
		total += estimateContent(m.Content)
		total += estimateContent(m.ReasoningContent)
		if m.ToolCallID != "" {
			total += tokens.Estimate(m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				total += 12 // assistant tool_call framing
				total += tokens.Estimate(tc.ID)
				total += tokens.Estimate(tc.Function.Name)
				total += estimateJSONContent(tc.Function.Arguments)
			}
		}
		// Images are billed by provider-specific token accounting. Reserve a
		// bounded estimate based on payload size instead of silently ignoring
		// them; the exact figure remains provider-specific.
		for _, img := range m.Images {
			total += 1024 + len(img.Data)/4096
		}
	}
	return total
}

// estimateContent applies the conservative text heuristic and adds a small
// fixed per-message margin. Empty content still costs an envelope.
func estimateContent(s string) int {
	if s == "" {
		return 0
	}
	return tokens.Estimate(s)
}

// estimateJSONContent accounts for request/tool payloads whose punctuation and
// escaped characters tokenize denser than ordinary prose.
func estimateJSONContent(s string) int {
	if s == "" {
		return 0
	}
	if !json.Valid([]byte(s)) {
		return tokens.Estimate(s)
	}
	runes := []rune(s)
	cjk := 0
	for _, r := range runes {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3040 && r <= 0x30FF) || (r >= 0xAC00 && r <= 0xD7AF) {
			cjk++
		}
	}
	other := len(runes) - cjk
	return cjk + (other+jsonToolEnvelopeDensityDivisor-1)/jsonToolEnvelopeDensityDivisor
}

func estimateToolDescriptor(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	return estimateJSONContent(string(raw))
}

// UncompactableContextError is surfaced when the minimal valid model context
// cannot fit the selected request budget. It carries diagnostics but never
// embeds full tool payloads.
type UncompactableContextError struct {
	EstimatedTokens int
	InputBudget     int
	Reason          string
}

func (e *UncompactableContextError) Error() string {
	if e == nil {
		return "uncompactable context"
	}
	if e.Reason == "" {
		return fmt.Sprintf("uncompactable context: estimated %d input tokens exceed budget %d", e.EstimatedTokens, e.InputBudget)
	}
	return fmt.Sprintf("uncompactable context (%s): estimated %d input tokens exceed budget %d", e.Reason, e.EstimatedTokens, e.InputBudget)
}

// RequestTooLargeError is a local preflight rejection, separate from a provider
// context-overflow error observed after a request was sent.
type RequestTooLargeError struct {
	EstimatedTokens int
	InputBudget     int
}

func (e *RequestTooLargeError) Error() string {
	return fmt.Sprintf("request input estimate %d exceeds model input budget %d", e.EstimatedTokens, e.InputBudget)
}

// InvalidRequestBudgetError marks a catalog entry whose context window cannot
// accommodate its own completion quota and the safety margin.
type InvalidRequestBudgetError struct {
	ModelWindow       int
	CompletionReserve int
	SafetyMargin      int
}

func (e *InvalidRequestBudgetError) Error() string {
	return fmt.Sprintf("model context window %d cannot accommodate completion reservation %d and safety margin %d",
		e.ModelWindow, e.CompletionReserve, e.SafetyMargin)
}

// sanitizeBudgetReason keeps diagnostic reasons bounded and single-line.
func sanitizeBudgetReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}
