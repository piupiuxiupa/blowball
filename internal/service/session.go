package service

import (
	"context"
	"encoding/json"
	"errors"
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

// SessionService owns session lifecycle and the Redis-first write-behind
// message persistence. It is safe for concurrent use: every public method
// takes a context, has no shared mutable state, and pushes straight through
// to the underlying stores.
type SessionService struct {
	mysql MySQLStore
	redis RedisStore
	fs    FSStore
	drain func(ctx context.Context) error
	nudge func()
}

// NewSessionService wires a SessionService from the bundled deps. The same
// SessionDeps value can be shared with NewMessageService and NewTitleService.
func NewSessionService(deps SessionDeps) *SessionService {
	return &SessionService{
		mysql: deps.MySQL,
		redis: deps.Redis,
		fs:    deps.FS,
		drain: deps.DrainMessageQueue,
		nudge: deps.NudgeMessageFlush,
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

// ErrSessionNotFound is returned by DeleteSession when the session does not
// exist or does not belong to the caller. Both cases map to the same sentinel
// (and the same HTTP 404) so existence of another user's session is never
// leaked. Handlers MUST distinguish this with errors.Is rather than
// string-matching.
var ErrSessionNotFound = errors.New("session not found")

// messageDrainTimeout bounds the synchronous write-behind drain invoked
// before archiving a deleted session and on a Redis read miss. Neither call
// site is an interactive hot path (DELETE is rare; a cache miss means 24h of
// idleness or a Redis restart), but both must eventually give up rather than
// hang on a wedged drain.
const messageDrainTimeout = 5 * time.Second

// DeleteSession removes a session the caller owns, in three steps:
//
//  1. Synchronously drain the write-behind ingest queue (bounded) so this
//     session's messages still awaiting their MySQL flush land BEFORE the
//     archive transaction snapshots the rows — otherwise the *_deleted
//     mirrors would silently miss everything sitting in the queue. A drain
//     failure aborts the delete: archiving an incomplete session is exactly
//     the data loss the caller avoids by retrying.
//  2. Atomically archive sessions/titles/messages into the *_deleted mirror
//     tables and delete the live sessions row (cascade clears live
//     titles/messages) in one MySQL transaction.
//  3. Proactively clear the Redis cache keys (msgs:{id}, session:{id}) —
//     database first, cache second. A stale cache key was already
//     unreachable (every read re-validates ownership against MySQL), so a
//     clear failure is logged and the key simply ages out on TTL.
//
// Ownership is validated first via GetSessionByID; a missing or non-owned
// session returns ErrSessionNotFound without touching any tier — and without
// draining anything.
func (s *SessionService) DeleteSession(ctx context.Context, userID, sessionID string) error {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "session.delete"),
		zap.String("user_id", userID),
		zap.String("session_id", sessionID),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	sess, err := s.mysql.GetSessionByID(ctx, sessionID)
	if err != nil {
		log.Error("session lookup failed", zap.Error(err))
		return fmt.Errorf("session.delete: lookup: %w", err)
	}
	if sess == nil || sess.UserID != userID {
		return ErrSessionNotFound
	}

	// 1) Bounded synchronous drain: unflushed messages must be in MySQL
	// before the archive transaction reads them out.
	if s.drain != nil {
		drainCtx, cancel := context.WithTimeout(ctx, messageDrainTimeout)
		defer cancel()
		if err := s.drain(drainCtx); err != nil {
			log.Error("pre-delete message queue drain failed; aborting delete", zap.Error(err))
			return fmt.Errorf("session.delete: drain: %w", err)
		}
	}

	// 2) Atomic archive + purge.
	if err := s.mysql.DeleteSession(ctx, sessionID); err != nil {
		log.Error("archive and purge failed", zap.Error(err))
		return fmt.Errorf("session.delete: purge: %w", err)
	}

	// 3) Cache clear, best-effort (db already deleted; TTL covers failures).
	if err := s.redis.ClearMessages(ctx, sessionID); err != nil {
		log.Warn("redis msgs cache clear failed; key expires on TTL", zap.Error(err))
	}
	if err := s.redis.DelSessionCache(ctx, sessionID); err != nil {
		log.Warn("redis session cache clear failed; key expires on TTL", zap.Error(err))
	}
	log.Info("session deleted")
	return nil
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

// SaveMessage persists a single message through the Redis-first write-behind
// path; see SaveMessagesBatch for the tier semantics.
func (s *SessionService) SaveMessage(ctx context.Context, userID string, msg model.Message) error {
	return s.SaveMessagesBatch(ctx, userID, []model.Message{msg})
}

// SaveMessagesBatch persists msgs through the Redis-first write-behind path:
// every message is stamped with a freshly minted client_msg_id (the
// idempotency key), and the whole batch is dual-written in ONE Redis
// transactional pipeline — onto the per-session read cache (msgs:{session_id},
// TTL refreshed) AND the global ingest queue (msgs:buffer, no TTL). A
// background flusher (internal/msgflush, agent/all roles) later moves the
// queue into MySQL in batches and refreshes sessions.update_time; no
// synchronous MySQL write and no filesystem write happens on this path.
//
// Failure policy: when the dual-write pipeline fails (Redis unavailable), the
// batch falls back to a synchronous direct MySQL write (idempotent INSERT
// IGNORE plus one update_time refresh) so the "messages always win"
// convention survives a Redis outage. A fallback failure means both tiers are
// down; it is logged loudly but not surfaced, keeping the SSE response path
// unblocked per the session-management spec.
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

	// Mint the idempotency key for every message. An already-stamped message
	// (test fixture, caller reuse) keeps its key so redelivery still
	// collapses to one row.
	for i := range msgs {
		if msgs[i].ClientMsgID != "" {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			log.Error("mint client_msg_id failed", zap.Int("index", i), zap.Error(err))
			return fmt.Errorf("session.save_messages_batch: mint client_msg_id: %w", err)
		}
		msgs[i].ClientMsgID = id.String()
	}

	// One canonical JSON blob flows to both keys of the dual write.
	raws := make([][]byte, 0, len(msgs))
	for i := range msgs {
		raw, err := json.Marshal(msgs[i])
		if err != nil {
			log.Error("marshal message failed", zap.Int("index", i), zap.Error(err))
			return fmt.Errorf("session.save_messages_batch: marshal: %w", err)
		}
		raws = append(raws, raw)
	}

	if err := s.redis.AppendMessagesDual(ctx, msgs[0].SessionID, raws); err != nil {
		log.Error("redis dual-write failed; falling back to synchronous MySQL write",
			zap.String("session_id", msgs[0].SessionID),
			zap.Error(err))
		s.fallbackDirectWrite(ctx, log, msgs)
		return nil
	}

	// Wake the flusher (non-blocking): a signal lets it flush early once the
	// queue has crossed its batch threshold instead of waiting for the next
	// interval tick.
	if s.nudge != nil {
		s.nudge()
	}
	return nil
}

// fallbackDirectWrite persists msgs straight into MySQL when the Redis
// dual-write pipeline failed: one idempotent batch INSERT plus one
// update_time refresh for the batch's session (the flusher normally owns
// both, but it never sees a batch that never reached the queue). Failures
// are logged, not returned — the SSE streaming path is never blocked by a
// store hiccup.
func (s *SessionService) fallbackDirectWrite(ctx context.Context, log *zap.Logger, msgs []model.Message) {
	if _, err := s.mysql.AppendMessages(ctx, msgs); err != nil {
		log.Error("mysql fallback write failed; batch lost",
			zap.String("session_id", msgs[0].SessionID),
			zap.Int("rows", len(msgs)),
			zap.Error(err))
		return
	}
	if err := s.mysql.UpdateSessionTime(ctx, msgs[0].SessionID); err != nil {
		log.Error("update session time failed", zap.String("session_id", msgs[0].SessionID), zap.Error(err))
	}
}

// SaveTurnUsage persists one turn's per-agent token cost into the turn_usage
// table. It is a thin wrapper over the MySQL store so the handler does not
// import the store package directly. The caller (the streaming handler)
// invokes this AFTER SaveMessagesBatch in the persist goroutine.
//
// Decision (task 3.5 / design D2): turn_usage is written as an INDEPENDENT
// call, NOT inside the SaveMessagesBatch transaction. The MySQL store layer
// has no transaction plumbing (every store method is a standalone Exec), and
// the turn-cost-tracking spec mandates that a usage write failure MUST NOT
// roll back the message batch (usage is observability data, messages are
// business data). A same-transaction write would require a savepoint to honor
// that priority, so an independent call is both simpler and faithful to the
// "messages win" priority convention. Recorded here per task 3.5.
func (s *SessionService) SaveTurnUsage(ctx context.Context, tu model.TurnUsage) error {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "session.save_turn_usage"),
		zap.String("session_id", tu.SessionID),
		zap.String("trace_id", tu.TraceID),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}
	if err := s.mysql.SaveTurnUsage(ctx, tu); err != nil {
		log.Error("save turn_usage failed", zap.Error(err))
		return fmt.Errorf("session.save_turn_usage: %w", err)
	}
	return nil
}
