// Package reqctx holds the per-request identity context values that must be
// readable across layer boundaries (pkg/logger, internal/tool) without those
// packages importing the layer that mints the value.
//
// It is the shared home of the session-id context key (tool-call-log-
// correlation capability): internal/agent's WithSessionID/
// SessionIDFromContext delegate here, so the turn path injects the id once at
// the streaming-handler entry and every downstream log site (logger.
// FromContext) and tool-layer consumer reads the SAME key. The package is a
// dependency-free leaf so both directions (agent → here, tool/logger → here)
// stay acyclic.
package reqctx

import "context"

// sessionCtxKey is the unexported context key type for the session_id value.
type sessionCtxKey struct{}

// WithSessionID returns a copy of ctx carrying sessionID so raw capture (and
// any future per-session consumer) can attribute work to its session without
// threading the id through every API. MessageStreamHandler injects it once
// per request; TitleService injects it into its background context. Empty
// sessionID returns ctx unchanged (matching trace.WithContext).
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// SessionIDFromContext returns the session_id stored in ctx, or an empty
// string when none is present. It is safe to call on a context that was never
// decorated by WithSessionID, and on a nil ctx.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionCtxKey{}).(string)
	return v
}
