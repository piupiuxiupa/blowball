// Package service implements blowball's business logic. SessionService owns
// three-layer message persistence and session lifecycle; MessageService owns
// the Redis -> FS -> MySQL fallback read chain; TitleService owns async title
// generation.
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
	ListSessionsWithTitle(ctx context.Context, userID string) ([]mysql.SessionWithTitle, error)
	UpsertTitle(ctx context.Context, t model.Title) error
	GetTitle(ctx context.Context, sessionID string) (*model.Title, error)
	AppendMessage(ctx context.Context, m model.Message) (int64, error)
	AppendMessages(ctx context.Context, msgs []model.Message) ([]int64, error)
	ListMessages(ctx context.Context, sessionID string) ([]model.Message, error)
}

// RedisStore is the subset of the cache layer that SessionService /
// MessageService call. It is satisfied by *redis.Store.
type RedisStore interface {
	AppendMessage(ctx context.Context, sessionID string, raw []byte) error
	AppendMessages(ctx context.Context, sessionID string, raws [][]byte) error
	GetMessages(ctx context.Context, sessionID string) ([][]byte, error)
	SetMessages(ctx context.Context, sessionID string, raws [][]byte) error
}

// FSStore is the subset of the file-system store that SessionService /
// MessageService call. It is satisfied by *fs.Store.
type FSStore interface {
	WriteSession(ctx context.Context, userID, sessionID string, data []byte) error
	ReadSession(ctx context.Context, userID, sessionID string) ([]byte, error)
	DeleteSession(ctx context.Context, userID, sessionID string) error
	EnsureUserDirs(ctx context.Context, userID string) error
}

// SessionDeps bundles the three store dependencies every service in this
// package needs. Phase 9 handlers / Phase 10 bootstrap construct one of these
// and hand it to NewSessionService, NewMessageService and NewTitleService.
type SessionDeps struct {
	MySQL MySQLStore
	Redis RedisStore
	FS    FSStore
}
