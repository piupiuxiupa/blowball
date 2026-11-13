package redis

import (
	"context"
)

// The message write-behind queue keys are RETIRED storage
// (refactor-message-storage): persistence is a direct idempotent MySQL
// transaction, and Redis only accelerates reads via the per-session cache
// (msgs:{session_id}, TTL-bearing — see message.go). The constants and claim
// helpers below remain temporarily for operator drains of pre-migration
// residue.
const (
	// messageBufferKey is the global FIFO of canonical message JSON blobs
	// awaiting insertion into MySQL.
	messageBufferKey = "msgs:buffer"
	// messageProcessingKey holds records atomically claimed by an in-flight
	// flusher batch. Records stay here until the batch's INSERT succeeds
	// (then they are LREM-acked) or the owning process crashes (then the next
	// startup's RecoverProcessingToBuffer requeues them).
	messageProcessingKey = "msgs:processing"
)

// AppendMessagesCache appends raws to the per-session read cache
// (msgs:{session_id}, TTL refreshed) in a single pipeline. An empty raws
// slice is a no-op. Cache-only by design (refactor-message-storage): the
// ingest queue is retired; a cache failure just means a later read backfills
// from MySQL.
func (s *Store) AppendMessagesCache(ctx context.Context, sessionID string, raws [][]byte) error {
	if len(raws) == 0 {
		return nil
	}
	key := messagesKey(sessionID)
	logCmd(ctx, "msgs.append_dual", key)

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
