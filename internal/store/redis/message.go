package redis

import "context"

// messagesKey formats the cache key for the message list of sessionID.
func messagesKey(sessionID string) string { return "msgs:" + sessionID }

// AppendMessage appends a single serialised message (raw) to the end of the
// cached message list for sessionID using RPUSH. The key's TTL is refreshed so
// a long-lived session whose messages arrive one at a time does not silently
// expire mid-conversation.
func (s *Store) AppendMessage(ctx context.Context, sessionID string, raw []byte) error {
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.append", key)

	pipe := s.client.TxPipeline()
	pipe.RPush(ctx, key, raw)
	pipe.Expire(ctx, key, s.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// GetMessages returns the cached message list for sessionID in insertion order
// (LRANGE 0 -1). An empty slice is returned when the list does not exist or is
// empty — both manifest as a zero-length result from LRANGE.
func (s *Store) GetMessages(ctx context.Context, sessionID string) ([][]byte, error) {
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.get", key)

	res, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(res))
	for i := range res {
		// Each element is the bytes we RPUSHed. Copy into a fresh slice so the
		// caller cannot accidentally alias internal redis buffers.
		b := []byte(res[i])
		out = append(out, b)
	}
	return out, nil
}

// SetMessages replaces the cached message list for sessionID with raws. The
// replacement is atomic from the caller's perspective: DEL + RPUSH are issued
// in a single transactional pipeline so concurrent readers never observe a
// half-populated list. An empty raws slice leaves the key deleted.
func (s *Store) SetMessages(ctx context.Context, sessionID string, raws [][]byte) error {
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.set", key)

	pipe := s.client.TxPipeline()
	pipe.Del(ctx, key)
	if len(raws) > 0 {
		// redis-go accepts a variadic of values; build a []any to avoid an
		// extra copy at the call site.
		members := make([]any, len(raws))
		for i := range raws {
			members[i] = raws[i]
		}
		pipe.RPush(ctx, key, members...)
		pipe.Expire(ctx, key, s.ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// ClearMessages removes the cached message list for sessionID entirely. A
// missing key is a no-op.
func (s *Store) ClearMessages(ctx context.Context, sessionID string) error {
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.clear", key)
	return s.client.Del(ctx, key).Err()
}
