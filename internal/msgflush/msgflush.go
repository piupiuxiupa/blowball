// Package msgflush implements the write-behind persistence path for chat
// messages (message-write-behind capability): message batches are staged in
// the Redis ingest queue (msgs:buffer, dual-written together with the
// msgs:{session_id} read cache by SessionService) and a background Flusher
// moves them into the MySQL messages table in batches.
//
// Delivery semantics are stricter than the llm-raw-capture path this package
// is modelled on (internal/llmraw), because messages are business data, not
// observability data: records are claimed atomically (LMOVE buffer→
// processing), inserted idempotently (INSERT IGNORE on the UNIQUE
// client_msg_id), and acknowledged per record (LREM). A failed insert parks
// the batch in msgs:processing and retries forever; the single dead-letter
// exception is a MySQL foreign-key error (1452 — the session was deleted),
// which can never succeed. Process startup requeues any processing residue.
//
// The package defines its own narrow store ports (Buffer / MessageStore) so
// production wiring passes the concrete *redis.Store / *mysql.Store while
// tests substitute in-memory fakes.
package msgflush

import (
	"context"
	"errors"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/lush/blowball/internal/model"
)

// Buffer is the Redis-side port of the write-behind path, satisfied by
// *redis.Store (see internal/store/redis/msgqueue.go).
type Buffer interface {
	// MoveMessageToProcessing atomically claims the head record of msgs:buffer
	// into msgs:processing (LMOVE) and returns it; nil, nil means the queue is
	// drained.
	MoveMessageToProcessing(ctx context.Context) ([]byte, error)
	// RemoveFromProcessing acknowledges one claimed record (LREM by value).
	RemoveFromProcessing(ctx context.Context, raw []byte) error
	// RecoverProcessingToBuffer requeues every processing record back onto the
	// buffer head in claim order (crash recovery).
	RecoverProcessingToBuffer(ctx context.Context) error
	// LenMessageBuffer returns the current buffer length (LLEN).
	LenMessageBuffer(ctx context.Context) (int64, error)
}

// MessageStore is the MySQL-side port, satisfied by *mysql.Store (see
// internal/store/mysql/message.go and session.go).
type MessageStore interface {
	// AppendMessages batch-inserts rows with INSERT IGNORE semantics on the
	// UNIQUE client_msg_id index, so redelivery collapses to one row.
	AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error)
	// UpdateSessionTime touches sessions.update_time for one session.
	UpdateSessionTime(ctx context.Context, sessionID string) error
}

// Config carries the operator-tunable flush parameters (the config.yaml
// `messages:` block — see internal/config). Zero values are replaced by the
// defaults below at Flusher construction; the config layer rejects explicit
// non-positive values at load time.
type Config struct {
	// Interval is the ticker bounding how long a queued message can sit in
	// the buffer before it lands in MySQL. Default 1s — shorter than llmraw's
	// 5s because the history API is sensitive to message visibility.
	Interval time.Duration
	// BatchSize caps the records claimed per round and doubles as the
	// queue-length threshold that triggers an early flush between ticks.
	BatchSize int
}

// Default config values, applied by NewFlusher when the corresponding field
// is zero. They mirror the documented config defaults so a hand-built Flusher
// (tests, Drain) behaves like a fully configured one.
const (
	DefaultFlushInterval  = time.Second
	DefaultFlushBatchSize = 100
)

// fkErrorCode is MySQL error 1452 (ER_NO_REFERENCED_ROW_2): "Cannot add or
// update a child row: a foreign key constraint fails". For the flusher it
// means the message's session row no longer exists, so the insert can never
// succeed — the one legitimately dead-letterable failure.
const fkErrorCode = 1452

// isFKError reports whether err is the MySQL foreign-key failure described
// above.
func isFKError(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == fkErrorCode
}
