// Package trace generates per-request trace identifiers.
//
// A trace_id is a UUID v7 string. It is intended to be generated at the HTTP
// middleware boundary and threaded through context so logs, store calls and
// agent invocations can all reference it. UUID v7 provides time-ordered
// identifiers that improve log sorting and database index locality.
package trace

import "github.com/google/uuid"

// New returns a fresh UUID v7 trace_id string. On the extremely unlikely
// failure to generate a UUID v7, it falls back to a UUID v4 so the caller
// always receives a valid identifier.
func New() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
