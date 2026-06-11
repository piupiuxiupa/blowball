// Package redis provides the cache layer of the blowball three-layer message
// store. It owns two key families:
//
//   - session:{session_id} — per-session blob cache (the warm layer)
//   - msgs:{session_id}    — per-session ordered message-list cache (RPUSH/LRANGE)
//
// A single *redis.Client and a uniform TTL is shared across both families so
// callers can use one Store value for everything.
package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/pkg/trace"
)

// Store wraps a *redis.Client and the TTL applied to session-level cache keys.
// The message-list keys (msgs:*) share the same TTL on write via SetMessages
// (see SetMessages doc for the rationale).
type Store struct {
	client *redis.Client
	ttl    time.Duration
}

// New connects to the Redis server at addr (using password and the logical DB
// index), pings it to confirm reachability, and returns a ready Store. ttl is
// the expiration applied to cached session entries.
func New(addr, password string, db int, ttl time.Duration) (*Store, error) {
	cli := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Ping(ctx).Err(); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Store{client: cli, ttl: ttl}, nil
}

// Close releases the underlying redis connection pool.
func (s *Store) Close() error {
	if s.client == nil {
		return nil
	}
	return s.client.Close()
}

// Client exposes the wrapped *redis.Client for callers that need direct access
// (e.g. integration test tooling). Service code should prefer the typed methods.
func (s *Store) Client() *redis.Client {
	return s.client
}

// TTL returns the configured cache TTL.
func (s *Store) TTL() time.Duration { return s.ttl }

// logCmd emits a debug log describing the about-to-be-run redis command,
// attaching trace_id from ctx when present.
func logCmd(ctx context.Context, op, key string) {
	fields := []zap.Field{
		zap.String("op", op),
		zap.String("key", key),
	}
	if tid := trace.FromContext(ctx); tid != "" {
		fields = append(fields, zap.String("trace_id", tid))
	}
	logger.L().Debug("redis command", fields...)
}
