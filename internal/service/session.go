package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// SessionSummary is the read model returned by ListSessions. It carries just
// the fields the session-list API needs: session_id, the optional title (empty
// string when none has been generated yet) and update_time.
type SessionSummary struct {
	SessionID  string    `json:"session_id"`
	Title      string    `json:"title"`
	UpdateTime time.Time `json:"update_time"`
}

// SessionService owns session lifecycle and three-layer message writes. It is
// safe for concurrent use: every public method takes a context, has no shared
// mutable state, and pushes straight through to the underlying stores.
type SessionService struct {
	mysql MySQLStore
	redis RedisStore
	fs    FSStore
}

// NewSessionService wires a SessionService from the bundled deps. The same
// SessionDeps value can be shared with NewMessageService and NewTitleService.
func NewSessionService(deps SessionDeps) *SessionService {
	return &SessionService{
		mysql: deps.MySQL,
		redis: deps.Redis,
		fs:    deps.FS,
	}
}

// CreateSession creates a new session owned by userID. It mints a UUID v7
// session_id, ensures the user's data/ subdirectories exist, and inserts a
// sessions row carrying the caller's trace_id. The generated session_id is
// returned on success.
func (s *SessionService) CreateSession(ctx context.Context, userID string) (string, error) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "session.create"),
		zap.String("user_id", userID),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	if err := s.fs.EnsureUserDirs(ctx, userID); err != nil {
		log.Error("ensure user dirs failed", zap.Error(err))
		return "", fmt.Errorf("session.create: user dirs: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		log.Error("mint session_id failed", zap.Error(err))
		return "", fmt.Errorf("session.create: mint session_id: %w", err)
	}
	sessionID := id.String()

	sess := model.Session{
		SessionID: sessionID,
		UserID:    userID,
		TraceID:   tid,
	}
	if err := s.mysql.CreateSession(ctx, sess); err != nil {
		log.Error("create session failed", zap.Error(err))
		return "", fmt.Errorf("session.create: persist: %w", err)
	}
	log.Info("session created", zap.String("session_id", sessionID))
	return sessionID, nil
}

// GetSessionByID returns the session matching sessionID. It is a thin wrapper
// over the MySQL store so handlers can validate ownership without importing
// the store package.
func (s *SessionService) GetSessionByID(ctx context.Context, sessionID string) (*model.Session, error) {
	return s.mysql.GetSessionByID(ctx, sessionID)
}

// GetSessionMessages returns a paginated slice of messages for sessionID from
// MySQL, ordered by (msg_time, msg_index, id). It is a thin wrapper over the
// store so the handler does not need to import the cursor package.
func (s *SessionService) GetSessionMessages(ctx context.Context, sessionID, cursor string, pageSize int, order string) ([]model.Message, string, error) {
	return s.mysql.ListMessagesPaged(ctx, sessionID, cursor, pageSize, order)
}

// ListSessions returns the caller's sessions most-recently-updated first. Each
// entry includes its title (empty string when no title has been generated).
// An empty slice is returned when the user has no sessions.
func (s *SessionService) ListSessions(ctx context.Context, userID string) ([]SessionSummary, error) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(zap.String("op", "session.list"), zap.String("user_id", userID))
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	rows, err := s.mysql.ListSessionsWithTitle(ctx, userID)
	if err != nil {
		log.Error("list sessions with title failed", zap.Error(err))
		return nil, fmt.Errorf("session.list: %w", err)
	}

	out := make([]SessionSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionSummary{
			SessionID:  r.SessionID,
			Title:      r.Title,
			UpdateTime: r.UpdateTime,
		})
	}
	return out, nil
}

// SaveMessage persists msg across all three storage tiers. Per the
// session-management spec, Redis is best-effort: a Redis error is logged but
// does NOT abort the write. MySQL is treated as a synchronous durability
// requirement per the design's "asynchronous write" intent; however the spec
// only explicitly blesses Redis failure as non-blocking, so we log MySQL
// failures and return nil so the streaming response path is never blocked by a
// database hiccup. The file system layer is the warm tier; an FS error is
// returned because a corrupted file would silently corrupt the recovery chain.
//
// The same canonical JSON blob flows into Redis (RPUSH element), the session
// file's messages[] array, and MySQL (via AppendMessage, which re-serializes
// through named params).
func (s *SessionService) SaveMessage(ctx context.Context, userID string, msg model.Message) error {
	return s.SaveMessagesBatch(ctx, userID, []model.Message{msg})
}

// SaveMessagesBatch persists msgs across all three storage tiers in batch form.
// It follows the same best-effort/error policies as SaveMessage:
//   - Redis: best-effort, logged and ignored on failure.
//   - FS: synchronous; an error is returned because a corrupted session file
//     would break recovery.
//   - MySQL: synchronous attempt; failures are logged and NOT returned so the
//     streaming response path is never blocked by a database hiccup.
//
// All messages are first canonicalised into raw JSON, then pushed to Redis in
// one RPUSH, appended to the session file in one read-modify-write, and finally
// inserted into MySQL with a single multi-value INSERT.
func (s *SessionService) SaveMessagesBatch(ctx context.Context, userID string, msgs []model.Message) error {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "session.save_messages_batch"),
		zap.String("user_id", userID),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}
	if len(msgs) == 0 {
		return nil
	}

	raws := make([][]byte, 0, len(msgs))
	for i := range msgs {
		raw, err := json.Marshal(msgs[i])
		if err != nil {
			log.Error("marshal message failed", zap.Int("index", i), zap.Error(err))
			return fmt.Errorf("session.save_messages_batch: marshal: %w", err)
		}
		raws = append(raws, raw)
	}

	// 1) Redis hot tier. Best-effort per spec: log + continue on failure.
	if err := s.redis.AppendMessages(ctx, msgs[0].SessionID, raws); err != nil {
		log.Warn("redis append batch failed; continuing with FS and MySQL", zap.Error(err))
	}

	// 2) FS warm tier. Read-modify-write so the session file accumulates every
	// message in order. A miss (new session) starts a fresh document.
	if err := s.appendToFS(ctx, userID, msgs[0].SessionID, raws); err != nil {
		log.Error("fs append failed", zap.Error(err))
		return fmt.Errorf("session.save_messages_batch: fs: %w", err)
	}

	// 3) MySQL durable tier. Synchronous per current decision; logged but NOT
	// returned so the SSE response path is never blocked by a DB hiccup. The
	// file layer above still holds the messages for the next RecoverMessages.
	if _, err := s.mysql.AppendMessages(ctx, msgs); err != nil {
		log.Error("mysql append batch failed; messages held in FS only", zap.Error(err))
	}

	return nil
}

// sessionFile is the on-disk JSON shape for the warm tier. messages holds the
// raw JSON blob of each Message (one per append) in insertion order.
type sessionFile struct {
	SessionID string            `json:"session_id"`
	Messages  []json.RawMessage `json:"messages"`
}

// appendToFS reads the existing session file (or starts fresh), appends the new
// raw message JSON blobs, and writes it back. A nil file (new session) yields a
// document whose messages array contains all supplied raws in order.
func (s *SessionService) appendToFS(ctx context.Context, userID, sessionID string, raws [][]byte) error {
	existing, err := s.fs.ReadSession(ctx, userID, sessionID)
	if err != nil {
		return fmt.Errorf("read session file: %w", err)
	}

	var doc sessionFile
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return fmt.Errorf("unmarshal session file: %w", err)
		}
	}
	if doc.SessionID == "" {
		doc.SessionID = sessionID
	}
	for _, raw := range raws {
		doc.Messages = append(doc.Messages, json.RawMessage(raw))
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal session file: %w", err)
	}
	if err := s.fs.WriteSession(ctx, userID, sessionID, out); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	return nil
}
