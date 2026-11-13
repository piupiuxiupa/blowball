package redis

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// compactionKey formats the cache key for a session's latest compaction
// record. The value is the JSON serialization of the latest
// context_compactions row (the latest-wins stitch source); a new compaction
// overwrites the whole key. TTL matches the msgs:/session: cache families so
// the key ages out on the same cadence.
func compactionKey(sessionID string) string { return "compaction:" + sessionID }

// SetCompactionCache caches data as the latest compaction record for
// sessionID, applying the configured TTL and overwriting any previous value.
func (s *Store) SetCompactionCache(ctx context.Context, sessionID string, data []byte) error {
	key := compactionKey(sessionID)
	logCmd(ctx, "compaction.set", key)
	return s.client.Set(ctx, key, data, s.ttl).Err()
}

// GetCompactionCache returns the cached latest-compaction blob for sessionID,
// or (nil, nil) when the key is absent (including the redis.Nil miss case).
func (s *Store) GetCompactionCache(ctx context.Context, sessionID string) ([]byte, error) {
	key := compactionKey(sessionID)
	logCmd(ctx, "compaction.get", key)

	res, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// DelCompactionCache removes the latest-compaction cache key for sessionID. A
// missing key is a no-op.
func (s *Store) DelCompactionCache(ctx context.Context, sessionID string) error {
	key := compactionKey(sessionID)
	logCmd(ctx, "compaction.del", key)
	return s.client.Del(ctx, key).Err()
}
