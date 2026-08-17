package redis

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// rawBufferKey is the write-behind buffer feeding the llm_raw_log MySQL table
// (llm-raw-capture capability). Producers RPUSH captured LLM payloads onto the
// tail; the background flusher (internal/llmraw) LPOPs them off the head in
// batches and inserts them into MySQL. Unlike the session/messages key
// families this key is a queue, not a cache: it carries NO TTL — an expiry
// would silently drop not-yet-flushed raw records — and its lifetime is
// bounded by the flusher draining it (plus, in the worst case, operator
// intervention when the flusher is down).
const rawBufferKey = "llm_raw:buffer"

// PushRawLog appends one serialised llm_raw_log record onto the buffer tail
// (RPUSH). It is called on the LLM streaming path, so callers keep it
// best-effort: an error here is logged by the caller and the record dropped —
// raw capture must never block a turn.
func (s *Store) PushRawLog(ctx context.Context, raw []byte) error {
	logCmd(ctx, "llm_raw.push", rawBufferKey)
	return s.client.RPush(ctx, rawBufferKey, raw).Err()
}

// PopRawLogs atomically removes up to count records from the buffer head
// (LPOP key count, Redis >= 6.2) and returns them in FIFO order. The pop is a
// single atomic command, so multiple flusher processes can consume the same
// buffer concurrently without ever handing the same record to two batches or
// letting an index-shift race drop records. An empty slice means the buffer
// is drained.
func (s *Store) PopRawLogs(ctx context.Context, count int64) ([][]byte, error) {
	logCmd(ctx, "llm_raw.pop", rawBufferKey)

	if count <= 0 {
		return nil, fmt.Errorf("redis: PopRawLogs: count must be positive, got %d", count)
	}
	res, err := s.client.LPopCount(ctx, rawBufferKey, int(count)).Result()
	if err != nil {
		// Real Redis replies with a nil array (go-redis surfaces it as
		// redis.Nil) when the list is empty or missing — that is a drained
		// buffer, not an error.
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	out := make([][]byte, 0, len(res))
	for i := range res {
		out = append(out, []byte(res[i]))
	}
	return out, nil
}

// LenRawLogs returns the current buffer length (LLEN). The flusher polls it
// to flush early once the queue crosses its batch threshold, instead of
// waiting for the interval ticker.
func (s *Store) LenRawLogs(ctx context.Context) (int64, error) {
	logCmd(ctx, "llm_raw.len", rawBufferKey)
	return s.client.LLen(ctx, rawBufferKey).Result()
}
