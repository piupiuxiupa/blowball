// Package middleware wires cross-cutting gin middlewares: per-request
// trace_id minting, JWT auth, and CORS.
package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/pkg/trace"
)

// TraceIDKey is the gin.Context key under which the per-request trace_id is
// published. Handlers and downstream code should read it through
// TraceIDFromCtx rather than touching this constant directly, but it is
// exported so other packages can stay in sync.
const TraceIDKey = "trace_id"

// TraceMiddleware generates a UUID trace_id for every request, publishes it on
// the gin.Context (TraceIDKey) and the standard context.Context (so
// trace.FromContext works inside services reached via c.Request.Context()),
// and echoes it back to the client on the X-Trace-Id response header.
func TraceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := trace.New()
		c.Set(TraceIDKey, id)
		c.Request = c.Request.WithContext(trace.WithContext(c.Request.Context(), id))
		c.Header("X-Trace-Id", id)
		c.Next()
	}
}

// TraceIDFromCtx returns the trace_id previously stashed on the gin.Context by
// TraceMiddleware, or an empty string when none is present.
func TraceIDFromCtx(c *gin.Context) string {
	v, _ := c.Get(TraceIDKey)
	id, _ := v.(string)
	return id
}

// traceIDOrNew returns the trace_id already on the gin.Context if one is
// present, otherwise mints a fresh UUID. Used by AuthMiddleware as a backstop
// when TraceMiddleware has not run upstream.
func traceIDOrNew(c *gin.Context) string {
	if id := TraceIDFromCtx(c); id != "" {
		return id
	}
	return trace.New()
}
