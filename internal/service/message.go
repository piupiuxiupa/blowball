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

// MessageService owns the read side of the three-layer store. RecoverMessages
// walks the Redis -> FS -> MySQL chain per the session-management spec,
// backfilling the upper tiers whenever a lower tier supplies the data. Writes
// are funnelled through SessionService.SaveMessage so the write path stays in
// exactly one place.
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

// RecoverMessages returns the full ordered message list for sessionID using the
// priority chain Redis -> FS -> MySQL, backfilling the upper tiers on each
// fallback so subsequent reads are cheaper. An empty (non-nil) slice with no
// error is returned for a brand-new session with no messages anywhere.
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

	// 1) Hot tier: Redis. Hit short-circuits FS and MySQL.
	if msgs, raws, err := s.tryRedis(ctx, log, sessionID); err != nil {
		log.Warn("redis recover failed; falling back to FS", zap.Error(err))
	} else if msgs != nil {
		return msgs, nil
	} else if len(raws) == 0 {
		log.Debug("redis miss")
	}

	// 2) Warm tier: FS file. On hit, parse messages and backfill Redis.
	if msgs, raws, err := s.tryFS(ctx, log, userID, sessionID); err != nil {
		log.Warn("fs recover failed; falling back to MySQL", zap.Error(err))
	} else if msgs != nil {
		return msgs, nil
	} else if len(raws) == 0 {
		log.Debug("fs miss")
	}

	// 3) Cold tier: MySQL. On hit, backfill Redis and FS.
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
	if err := s.writeFSFromRaws(ctx, userID, sessionID, raws); err != nil {
		log.Warn("backfill fs from mysql failed", zap.Error(err))
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

// tryFS returns parsed messages from the session file. On hit it backfills
// Redis (SetMessages) so the next reader hits the hot tier.
func (s *MessageService) tryFS(ctx context.Context, log *zap.Logger, userID, sessionID string) ([]model.Message, [][]byte, error) {
	data, err := s.deps.FS.ReadSession(ctx, userID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 {
		return nil, nil, nil
	}
	var doc sessionFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("unmarshal fs session file: %w", err)
	}
	if len(doc.Messages) == 0 {
		return nil, nil, nil
	}

	raws := make([][]byte, 0, len(doc.Messages))
	msgs := make([]model.Message, 0, len(doc.Messages))
	for _, r := range doc.Messages {
		raws = append(raws, []byte(r))
		var m model.Message
		if err := json.Unmarshal(r, &m); err != nil {
			return nil, nil, fmt.Errorf("unmarshal fs message: %w", err)
		}
		msgs = append(msgs, m)
	}
	log.Debug("recovered from fs", zap.Int("count", len(msgs)))

	if err := s.deps.Redis.SetMessages(ctx, sessionID, raws); err != nil {
		log.Warn("backfill redis from fs failed", zap.Error(err))
	}
	return msgs, raws, nil
}

// writeFSFromRaws writes a fresh session-file document from the supplied raw
// message blobs. Used when MySQL backfills the warm tier.
func (s *MessageService) writeFSFromRaws(ctx context.Context, userID, sessionID string, raws [][]byte) error {
	doc := sessionFile{SessionID: sessionID, Messages: make([]json.RawMessage, 0, len(raws))}
	for _, r := range raws {
		doc.Messages = append(doc.Messages, json.RawMessage(r))
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal session file: %w", err)
	}
	if err := s.deps.FS.WriteSession(ctx, userID, sessionID, out); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	return nil
}
