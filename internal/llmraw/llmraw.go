// Package llmraw implements the write-behind persistence path for raw LLM
// capture (llm-raw-capture capability): captured request/response/error
// payloads are staged in a Redis list (llm_raw:buffer) and a background
// flusher moves them into the MySQL llm_raw_log table in batches.
//
// Neither stage ever blocks or fails the chat path: the sink pushes with a
// short bounded timeout and drops (with a WARN) on failure; the flusher runs
// on its own goroutine, paced by an interval ticker plus a queue-length
// trigger, and drains what it can on graceful shutdown.
//
// The package defines its own narrow store ports (Buffer / RawLogStore) so
// production wiring passes the concrete *redis.Store / *mysql.Store while
// tests substitute in-memory fakes.
package llmraw

import (
	"context"

	"github.com/lush/blowball/internal/model"
)

// Buffer is the Redis-side port of the write-behind path, satisfied by
// *redis.Store (see internal/store/redis/llm_raw.go).
type Buffer interface {
	// PushRawLog appends one serialised record to the buffer tail.
	PushRawLog(ctx context.Context, raw []byte) error
	// PopRawLogs atomically removes up to count records from the head.
	PopRawLogs(ctx context.Context, count int64) ([][]byte, error)
	// LenRawLogs returns the current buffer length.
	LenRawLogs(ctx context.Context) (int64, error)
}

// RawLogStore is the MySQL-side port, satisfied by *mysql.Store (see
// internal/store/mysql/llm_raw_log.go).
type RawLogStore interface {
	// AppendRawLogs batch-inserts rows, isolating per-row failures.
	AppendRawLogs(ctx context.Context, logs []model.LLMRawLog) error
}
