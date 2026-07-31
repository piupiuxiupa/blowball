package agent

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/stream"
)

// transientErrorSignals are substrings that, when present in an error message,
// mark it as a transient (retryable) failure. The LLM client wraps SDK errors
// with messages like "liang: stream chat: <underlying>"; the underlying openai-go
// error string carries the HTTP status (e.g. "429 Too Many Requests") or a
// transport error ("context deadline exceeded", "connection reset"). This
// pattern-based classifier keeps the agent package SDK-agnostic (it does not
// import openai-go error types), matching how general-purpose retry libraries
// classify by string when typed errors are unavailable.
var transientErrorSignals = []string{
	"429",
	"rate limit",
	"rate_limit",
	"timeout",
	"timed out",
	"time out",
	"context deadline exceeded",
	"connection reset",
	"connection refused",
	"eof", // truncated stream — often transient
	"502",
	"503",
	"504",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
	"temporarily",
	"try again",
}

// isTransientError reports whether err represents a transient (retryable)
// failure: LLM 429/5xx/timeout/network blips. Semantic errors (bad_args,
// unknown_tool) and non-matching errors are NOT transient and are never
// retried. It unwraps joined errors so wrapped messages are inspected.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range transientErrorSignals {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// computeBackoff returns the delay before the given retry attempt number
// (1-based: attempt 1 is the first RETRY after the initial call). It applies
// exponential backoff (initial * 2^(attempt-1)) capped at MaxBackoff. attempt
// must be >= 1.
func computeBackoff(policy config.AgentRetryConfig, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	initial := policy.InitialBackoff
	if initial <= 0 {
		initial = config.DefaultRetryInitialBackoff()
	}
	max := policy.MaxBackoff
	if max <= 0 {
		max = config.DefaultRetryMaxBackoff()
	}
	d := initial
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > max {
			d = max
			break
		}
	}
	return d
}

// errRetryBudgetExceeded is returned internally when the per-turn retry token
// budget has been consumed; it stops further retries.
var errRetryBudgetExceeded = errors.New("retry budget exceeded")

// retryBudget bounds the total tokens spent on sub-agent retries within one
// Confucius turn (capability C). It is concurrency-safe because parallel
// dispatches share it. A limit of 0 means unlimited (no budget enforced).
type retryBudget struct {
	mu     sync.Mutex
	limit  int
	spent  int
}

// newRetryBudget builds a budget with the given token limit. limit <= 0 means
// unlimited (allows() always returns true, charge() is a no-op).
func newRetryBudget(limit int) *retryBudget {
	return &retryBudget{limit: limit}
}

// charge records tokens consumed by a (failed) retry attempt against the
// turn budget. Safe to call concurrently.
func (b *retryBudget) charge(u Usage) {
	if b == nil || b.limit <= 0 {
		return
	}
	b.mu.Lock()
	b.spent += u.TotalTokens
	b.mu.Unlock()
}

// allows reports whether the budget permits at least one more retry. It always
// returns true when the budget is unlimited (limit <= 0).
func (b *retryBudget) allows() bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent < b.limit
}

// retryErrorEvent builds an agent_error event that signals an in-progress retry
// to the frontend: it carries the standard error_code plus Meta.retry=true so
// the UI can distinguish "we are retrying this sub-agent" from a terminal
// failure. It is emitted BEFORE the backoff sleep that precedes the retry.
func retryErrorEvent(agent string, err error) stream.StreamEvent {
	e := stream.AgentErrorEvent(agent, err.Error(), "retry")
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta["retry"] = true
	return e
}
