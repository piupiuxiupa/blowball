package trace

import "context"

// ctxKey is the unexported context key type used to stash a trace_id value.
// Using a distinct type avoids collisions with keys defined elsewhere.
type ctxKey struct{}

// key is the canonical context key under which the trace_id string is stored.
var key = ctxKey{}

// WithContext returns a copy of ctx carrying the trace_id value. Middleware
// that mints the trace should call this once per request; downstream callers
// recover the id via FromContext.
func WithContext(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, key, traceID)
}

// FromContext returns the trace_id stored in ctx, or an empty string if none
// is present. It is safe to call on a context that was never decorated by
// WithContext.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(key).(string)
	return v
}
