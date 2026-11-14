package redis

import (
	"context"
)

// AppendMessagesCache appends raws to the per-session read cache
// (msgs:{session_id}, TTL refreshed) in a single pipeline. An empty raws
// slice is a no-op. Cache-only by design (refactor-message-storage): the
// write-behind ingest queue is retired; a cache failure just means a later
// read backfills from MySQL.
func (s *Store) AppendMessagesCache(ctx context.Context, sessionID string, raws [][]byte) error {
	if len(raws) == 0 {
		return nil
	}
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.append_cache", key)

	members := make([]any, len(raws))
	for i := range raws {
		members[i] = raws[i]
	}

	pipe := s.client.TxPipeline()
	pipe.RPush(ctx, key, members...)
	pipe.Expire(ctx, key, s.ttl)
	_, err := pipe.Exec(ctx)
	return err
}
