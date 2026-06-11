package redis

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// sessionKey formats the cache key for sessionID.
func sessionKey(sessionID string) string { return "session:" + sessionID }

// SetSessionCache writes data as the cached blob for sessionID, applying the
// configured TTL. The previous cached value (if any) is overwritten.
func (s *Store) SetSessionCache(ctx context.Context, sessionID string, data []byte) error {
	key := sessionKey(sessionID)
	logCmd(ctx, "session.set", key)
	return s.client.Set(ctx, key, data, s.ttl).Err()
}

// GetSessionCache returns the cached blob for sessionID, or (nil, nil) when
// the session is not in cache (including the redis.Nil miss case).
func (s *Store) GetSessionCache(ctx context.Context, sessionID string) ([]byte, error) {
	key := sessionKey(sessionID)
	logCmd(ctx, "session.get", key)

	res, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DelSessionCache removes the cached blob for sessionID. A missing key is a
// no-op (Del does not error on non-existent keys).
func (s *Store) DelSessionCache(ctx context.Context, sessionID string) error {
	key := sessionKey(sessionID)
	logCmd(ctx, "session.del", key)
	return s.client.Del(ctx, key).Err()
}
