// Package trace generates per-request trace identifiers.
//
// A trace_id is a UUID v4 string. It is intended to be generated at the HTTP
// middleware boundary and threaded through context so logs, store calls and
// agent invocations can all reference it.
package trace

import "github.com/google/uuid"

// New returns a fresh UUID v4 trace_id string.
func New() string {
	return uuid.NewString()
}
