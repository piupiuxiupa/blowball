// Package service implements blowball's business logic. SessionService owns
// the Redis-first write-behind message persistence and session lifecycle;
// MessageService owns the Redis → MySQL fallback read chain (with a
// write-behind drain in between); TitleService owns async title generation.
//
// Each service depends on small store interfaces defined in this file rather
// than importing the concrete store types. This keeps the services testable
// with in-memory fakes and lets the store packages evolve without churn here.
package service

import (
	"context"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/store/mysql"
)

// MySQLStore is the subset of the persistent store that SessionService /
// MessageService / TitleService call. It is satisfied by *mysql.Store.
type MySQLStore interface {
	CreateSession(ctx context.Context, sess model.Session) error
	GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error)
	DeleteSession(ctx context.Context, sessionID string) error
	UpdateSessionTime(ctx context.Context, sessionID string) error
	ListSessionsWithTitle(ctx context.Context, userID string) ([]mysql.SessionWithTitle, error)
	UpsertTitle(ctx context.Context, t model.Title) error
	UpsertTitleManual(ctx context.Context, t model.Title) error
	GetTitle(ctx context.Context, sessionID string) (*model.Title, error)
	// AppendMessages batch-inserts rows with INSERT IGNORE semantics on the
	// UNIQUE client_msg_id — the SaveMessagesBatch fallback direct-write path
	// relies on the same idempotency the background flusher does.
	AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error)
	ListMessages(ctx context.Context, sessionID string) ([]model.Message, error)
	ListMessagesPaged(ctx context.Context, sessionID, cursor string, pageSize int, order string) ([]model.Message, string, error)
	SaveTurnUsage(ctx context.Context, tu model.TurnUsage) error
}

// RedisStore is the subset of the cache layer that SessionService /
// MessageService call. It is satisfied by *redis.Store.
type RedisStore interface {
	// AppendMessagesDual is the write-behind dual write: one transactional
	// pipeline pushing the batch onto BOTH the per-session read cache
	// (msgs:{session_id}, TTL refreshed) and the global ingest queue
	// (msgs:buffer, no TTL).
	AppendMessagesDual(ctx context.Context, sessionID string, raws [][]byte) error
	GetMessages(ctx context.Context, sessionID string) ([][]byte, error)
	SetMessages(ctx context.Context, sessionID string, raws [][]byte) error
	ClearMessages(ctx context.Context, sessionID string) error
	DelSessionCache(ctx context.Context, sessionID string) error
}

// FSStore is the subset of the file-system store the services call. The
// Redis-first persistence change removed the session-file warm tier
// entirely; the FS store's remaining duty is the per-user directory layout
// (workspace/ and the reserved .blowball/skills/ namespace) that
// CreateSession ensures. It is satisfied by *fs.Store.
type FSStore interface {
	EnsureUserDirs(ctx context.Context, userID string) error
}

// SessionDeps bundles the store dependencies every service in this package
// needs. Phase 9 handlers / Phase 10 bootstrap construct one of these and
// hand it to NewSessionService, NewMessageService and NewTitleService.
//
// DrainMessageQueue is the synchronous write-behind drain primitive
// (msgflush.Drain bound to the concrete stores at wiring time). It is invoked
// with a bounded context before a session is archived for deletion (so the
// mirror tables capture messages still sitting in the ingest queue) and on a
// Redis read miss (so the cache backfill cannot erase unflushed rows). A nil
// value disables both call sites — production wiring always sets it, tests
// substitute a fake or leave it nil.
//
// NudgeMessageFlush (optional) is invoked after every successful dual write
// so the background flusher can flush early once the queue crosses its batch
// threshold instead of waiting for the interval ticker. Production wiring
// sets it to a non-blocking send on the flusher's shared notify channel; a
// nil value simply disables the early-flush path.
type SessionDeps struct {
	MySQL             MySQLStore
	Redis             RedisStore
	FS                FSStore
	DrainMessageQueue func(ctx context.Context) error
	NudgeMessageFlush func()
}
