package redis

import (
	"context"
	"encoding/json"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/lush/blowball/pkg/model"
)

func runtimeStatusKey(sessionID string) string { return "runtime_status:" + sessionID }
func runtimeStatusIDsKey(sessionID string) string {
	return "runtime_status_ids:" + sessionID
}

var appendRuntimeStatusScript = goredis.NewScript(`
if redis.call("SISMEMBER", KEYS[2], ARGV[1]) == 0 then
  redis.call("SADD", KEYS[2], ARGV[1])
  redis.call("RPUSH", KEYS[1], ARGV[2])
end
redis.call("PEXPIRE", KEYS[1], ARGV[3])
redis.call("PEXPIRE", KEYS[2], ARGV[3])
return 1
`)

// AppendRuntimeStatus appends an immutable entry once. Repeating a status_id
// refreshes the cache TTL without replacing content or duplicating the entry.
func (s *Store) AppendRuntimeStatus(ctx context.Context, sessionID string, status model.RuntimeStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal runtime status: %w", err)
	}
	key := runtimeStatusKey(sessionID)
	logCmd(ctx, "runtime_status.append", key)
	return appendRuntimeStatusScript.Run(ctx, s.client,
		[]string{key, runtimeStatusIDsKey(sessionID)},
		status.StatusID, raw, s.ttl.Milliseconds(),
	).Err()
}

// ListRuntimeStatus returns entries in append order. A malformed entry fails
// the whole read so callers can start a new cache epoch instead of replaying a
// partial status log.
func (s *Store) ListRuntimeStatus(ctx context.Context, sessionID string) ([]model.RuntimeStatus, error) {
	key := runtimeStatusKey(sessionID)
	logCmd(ctx, "runtime_status.list", key)
	raws, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]model.RuntimeStatus, 0, len(raws))
	for _, raw := range raws {
		var status model.RuntimeStatus
		if err := json.Unmarshal([]byte(raw), &status); err != nil {
			return nil, fmt.Errorf("decode runtime status: %w", err)
		}
		out = append(out, status)
	}
	return out, nil
}

// ClearRuntimeStatus removes both the ordered entries and their id registry.
func (s *Store) ClearRuntimeStatus(ctx context.Context, sessionID string) error {
	key := runtimeStatusKey(sessionID)
	logCmd(ctx, "runtime_status.clear", key)
	return s.client.Del(ctx, key, runtimeStatusIDsKey(sessionID)).Err()
}
