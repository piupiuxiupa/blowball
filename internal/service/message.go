package service

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// MessageService owns the read side of the two-layer store. RecoverMessages
// walks the Redis → MySQL chain per the session-management spec, inserting a
// bounded synchronous drain of the write-behind queue before the MySQL
// fallback so the cache backfill cannot erase rows that were persisted but
// not yet flushed. Writes are funnelled through SessionService.SaveMessage so
// the write path stays in exactly one place.
type MessageService struct {
	deps        SessionDeps
	saveMessage func(ctx context.Context, userID string, msg model.Message) error
}

// NewMessageService wires a MessageService. The save hook lets MessageService
// delegate to a SessionService rather than duplicating the orchestration; tests
// can substitute a no-op to keep the unit focused on the read path.
func NewMessageService(deps SessionDeps, save func(ctx context.Context, userID string, msg model.Message) error) *MessageService {
	return &MessageService{deps: deps, saveMessage: save}
}

// AppendMessage is a thin wrapper over SessionService.SaveMessage so callers
// that work purely with the MessageService still have a typed append entry
// point. The orchestration logic lives in SessionService.SaveMessage.
func (s *MessageService) AppendMessage(ctx context.Context, userID string, msg model.Message) error {
	if s.saveMessage == nil {
		return fmt.Errorf("message.append: save hook not configured")
	}
	return s.saveMessage(ctx, userID, msg)
}

// RecoverMessages returns the full ordered message list for sessionID using
// the priority chain Redis → MySQL, backfilling the cache on the MySQL
// fallback so subsequent reads are cheaper. The filesystem no longer
// participates.
//
// On a Redis miss (or a Redis error) a bounded synchronous drain of the
// write-behind queue runs first: messages dual-written but not yet flushed
// must land in MySQL BEFORE the ListMessages read, and especially before the
// SetMessages backfill — the DEL+RPUSH inside SetMessages would otherwise
// drop the unflushed rows from the cache layer, stretching the inconsistency
// window from one flush interval to the whole cache TTL. Misses only happen
// after 24h of idleness or a Redis restart, so the drain cost is negligible;
// a drain failure is logged and the read proceeds (the backfill race then
// self-heals on the next miss once the flusher catches up).
func (s *MessageService) RecoverMessages(ctx context.Context, userID, sessionID string) ([]model.Message, error) {
	tid := trace.FromContext(ctx)
	log := logger.L().With(
		zap.String("op", "message.recover"),
		zap.String("session_id", sessionID),
		zap.String("user_id", userID),
	)
	if tid != "" {
		log = log.With(zap.String("trace_id", tid))
	}

	// 1) Hot tier: Redis. A hit short-circuits the drain and the MySQL read.
	if msgs, _, err := s.tryRedis(ctx, log, sessionID); err != nil {
		log.Warn("redis recover failed; falling back to MySQL", zap.Error(err))
	} else if msgs != nil {
		return msgs, nil
	} else {
		log.Debug("redis miss")
	}

	// 2) Miss: drain the write-behind queue first (bounded, best-effort) so
	// the MySQL read below observes every persisted message.
	if s.deps.DrainMessageQueue != nil {
		drainCtx, cancel := context.WithTimeout(ctx, messageDrainTimeout)
		if err := s.deps.DrainMessageQueue(drainCtx); err != nil {
			log.Warn("write-behind drain before mysql fallback failed", zap.Error(err))
		}
		cancel()
	}

	// 3) Cold tier: MySQL. On hit, backfill the Redis cache.
	msgs, err := s.deps.MySQL.ListMessages(ctx, sessionID)
	if err != nil {
		log.Error("mysql recover failed", zap.Error(err))
		return nil, fmt.Errorf("message.recover: mysql: %w", err)
	}
	if len(msgs) == 0 {
		log.Debug("no messages in any tier (new session)")
		return []model.Message{}, nil
	}

	raws := make([][]byte, 0, len(msgs))
	for i := range msgs {
		b, mErr := json.Marshal(msgs[i])
		if mErr != nil {
			log.Error("marshal mysql message failed", zap.Error(mErr))
			return nil, fmt.Errorf("message.recover: marshal: %w", mErr)
		}
		raws = append(raws, b)
	}

	if err := s.deps.Redis.SetMessages(ctx, sessionID, raws); err != nil {
		log.Warn("backfill redis from mysql failed", zap.Error(err))
	}

	return msgs, nil
}

// tryRedis returns the parsed messages when the Redis tier has at least one
// entry. The returned `raws` lets the caller distinguish a true miss (empty
// raws, nil msgs, nil err) from a hit.
func (s *MessageService) tryRedis(ctx context.Context, log *zap.Logger, sessionID string) ([]model.Message, [][]byte, error) {
	raws, err := s.deps.Redis.GetMessages(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if len(raws) == 0 {
		return nil, nil, nil
	}
	msgs := make([]model.Message, 0, len(raws))
	for _, r := range raws {
		var m model.Message
		if err := json.Unmarshal(r, &m); err != nil {
			log.Warn("redis message unmarshal failed", zap.Error(err))
			return nil, nil, fmt.Errorf("unmarshal redis message: %w", err)
		}
		msgs = append(msgs, m)
	}
	log.Debug("recovered from redis", zap.Int("count", len(msgs)))
	return msgs, raws, nil
}
