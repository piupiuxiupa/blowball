package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// The message write-behind queue (message-write-behind capability). Producers
// dual-write each persisted message batch into BOTH the per-session read cache
// (msgs:{session_id}, TTL-bearing — see message.go) and the global ingest
// queue below; the background flusher (internal/msgflush) then moves queued
// records into the MySQL messages table.
//
// Unlike the cache key families these two keys form a reliable queue, not a
// cache: they carry NO TTL — an expiry would silently drop not-yet-flushed
// messages, which are business data — and their lifetime is bounded by the
// flusher draining them (plus, in the worst case, operator intervention when
// the flusher is down). The claim protocol is LMOVE buffer→processing +
// LREM-ack, so multiple flusher processes can consume the same queue
// concurrently without duplicating or losing records.
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

// AppendMessagesDual appends raws to BOTH the per-session read cache
// (msgs:{session_id}, TTL refreshed) and the global ingest queue
// (msgs:buffer, no TTL) in a single transactional pipeline. One round trip,
// and the two writes succeed or fail together: a pipeline failure means
// neither key was written, so the caller's fallback path (synchronous direct
// MySQL write) never has to reason about a half-applied dual write. An empty
// raws slice is a no-op. The queue elements are byte-identical to the cache
// elements — the same canonical message JSON blob.
func (s *Store) AppendMessagesDual(ctx context.Context, sessionID string, raws [][]byte) error {
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
	pipe.RPush(ctx, messageBufferKey, members...)
	_, err := pipe.Exec(ctx)
	return err
}

// PushMessageBuffer appends raws onto the ingest queue tail (RPUSH, no TTL)
// without touching the read cache. The production write path always goes
// through AppendMessagesDual; this bare producer exists for tooling and tests
// that need to stage queue records directly.
func (s *Store) PushMessageBuffer(ctx context.Context, raws [][]byte) error {
	if len(raws) == 0 {
		return nil
	}
	logCmd(ctx, "msgs.push_buffer", messageBufferKey)

	members := make([]any, len(raws))
	for i := range raws {
		members[i] = raws[i]
	}
	return s.client.RPush(ctx, messageBufferKey, members...).Err()
}

// MoveMessageToProcessing atomically claims the HEAD record of the ingest
// queue (LMOVE msgs:buffer msgs:processing LEFT RIGHT) and returns it. The
// claim is a single atomic command, so multiple flusher processes can consume
// the queue concurrently: each record moves into exactly one process's batch.
// A nil slice with a nil error means the queue is drained.
func (s *Store) MoveMessageToProcessing(ctx context.Context) ([]byte, error) {
	logCmd(ctx, "msgs.move_processing", messageBufferKey)

	res, err := s.client.LMove(ctx, messageBufferKey, messageProcessingKey, "LEFT", "RIGHT").Result()
	if err != nil {
		// An empty or missing source list surfaces as redis.Nil — a drained
		// queue, not an error.
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return []byte(res), nil
}

// RemoveFromProcessing acknowledges a claimed record: it removes the first
// occurrence of raw (the exact canonical JSON the flusher claimed) from the
// processing list. LREM scans from the head, which is O(list length) —
// acceptable because processing is emptied every round and only holds crash
// residue otherwise (bounded by batch size × crash count).
func (s *Store) RemoveFromProcessing(ctx context.Context, raw []byte) error {
	logCmd(ctx, "msgs.ack", messageProcessingKey)
	return s.client.LRem(ctx, messageProcessingKey, -1, raw).Err()
}

// LenMessageBuffer returns the current ingest-queue length (LLEN). The
// flusher polls it to flush early once the queue crosses its batch
// threshold, instead of waiting for the interval ticker.
func (s *Store) LenMessageBuffer(ctx context.Context) (int64, error) {
	logCmd(ctx, "msgs.len_buffer", messageBufferKey)
	return s.client.LLen(ctx, messageBufferKey).Result()
}

// AppendOnlyEnabled reports whether the Redis server has AOF persistence
// enabled (CONFIG GET appendonly). The ingest queue is the only synchronous
// durability layer for messages, so a bare-restart Redis without AOF loses
// whatever sits in the flush window; callers probe this at startup to WARN.
// An error (no CONFIG permission, managed Redis, unreachable server) is
// returned verbatim — probing is deliberately best-effort and never blocks
// startup.
func (s *Store) AppendOnlyEnabled(ctx context.Context) (bool, error) {
	logCmd(ctx, "server.appendonly_probe", "CONFIG GET appendonly")

	res, err := s.client.ConfigGet(ctx, "appendonly").Result()
	if err != nil {
		return false, err
	}
	v, ok := res["appendonly"]
	if !ok {
		return false, fmt.Errorf("redis: CONFIG GET appendonly returned no field")
	}
	return strings.EqualFold(fmt.Sprintf("%v", v), "yes"), nil
}

// RecoverProcessingToBuffer requeues every record parked in the processing
// list back onto the HEAD of the ingest queue, preserving the original claim
// order, and leaves processing empty. It is the crash-recovery step a process
// runs before entering its flush loop: records claimed by a previous process
// that died before insert-and-ack get redelivered (the UNIQUE client_msg_id
// index absorbs any record the crash interrupted AFTER its INSERT committed).
//
// The move takes the processing TAIL and pushes it onto the buffer HEAD
// (LMOVE RIGHT LEFT), repeated until processing is empty. Walking from the
// tail restores the head-first claim order: for processing [p1, p2, p3] the
// successive pushes build buffer [p1, p2, p3, ...fresh], so the next claims
// replay p1, p2, p3 in their original order. Per-element LMOVE keeps the
// recovery itself crash-safe: an interruption mid-recovery leaves a prefix
// requeued and the rest in processing, and the next run finishes the job.
func (s *Store) RecoverProcessingToBuffer(ctx context.Context) error {
	logCmd(ctx, "msgs.recover", messageProcessingKey)

	for {
		_, err := s.client.LMove(ctx, messageProcessingKey, messageBufferKey, "RIGHT", "LEFT").Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return nil // processing drained
			}
			return err
		}
	}
}
