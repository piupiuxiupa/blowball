package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lush/blowball/internal/run"
)

// Queue admission/continuation commands over the shared *redis.Client
// (session-message-queue capability; the run.Store queue primitives). The
// queue is the Redis LIST `queue:{sid}` holding JSON-encoded run.QueueEntry
// values in enqueue order; Redis auto-deletes the key when the list empties,
// so an absent key and an empty queue are the same state.
//
// The FIFO invariant — "queue non-empty ⇒ a new message may only RPUSH;
// claim free AND queue empty ⇒ a message may claim directly" — is sealed by
// admitScript, and the head's pop + claim transfer is sealed by pumpHeadScript.

func queueKey(sid string) string { return "queue:" + sid }

// admitScript atomically admits one message candidate:
//
//	claim free AND queue empty → claim it for ARGV[1] (the fast path,
//	                               byte-identical to a successful SET NX)
//	otherwise LLEN < maxDepth   → RPUSH ARGV[3] (the entry JSON), report depth
//	otherwise                   → full
//
// The claim-vs-queue decision and the RPUSH are one atomic step, so a message
// arriving in the release→pump window sees the queue non-empty and cannot
// jump the line by claiming directly.
const admitScript = `if not redis.call("GET", KEYS[1]) and redis.call("LLEN", KEYS[2]) == 0 then
	redis.call("SET", KEYS[1], ARGV[1], "EX", ARGV[4])
	return {"claimed", 0}
end
if redis.call("LLEN", KEYS[2]) < tonumber(ARGV[2]) then
	redis.call("RPUSH", KEYS[2], ARGV[3])
	return {"queued", redis.call("LLEN", KEYS[2])}
end
return {"full", 0}`

// pumpHeadScript atomically pops the queue head and transfers the session
// claim to it, but only when the caller's peeked entry (ARGV[1], the exact
// stored JSON) still is the head — another actor may have changed the queue
// between the Go-side peek and this script — and the claim is free. Returns 1
// when pumped, 0 when the queue is empty, and -1 when nothing changed (head
// moved or claim held; the head stays enqueued for the next pump attempt).
const pumpHeadScript = `local head = redis.call("LINDEX", KEYS[2], 0)
if not head then return 0 end
if head ~= ARGV[1] then return -1 end
if redis.call("GET", KEYS[1]) then return -1 end
redis.call("LPOP", KEYS[2])
redis.call("SET", KEYS[1], ARGV[2], "EX", ARGV[3])
return 1`

// AdmitMessage implements run.Store.
func (s *RunStore) AdmitMessage(ctx context.Context, sessionID string, entry run.QueueEntry, maxDepth int) (run.AdmitResult, error) {
	payload, err := json.Marshal(entry)
	if err != nil {
		return run.AdmitResult{}, fmt.Errorf("runstore: marshal queue entry: %w", err)
	}
	logCmd(ctx, "run.admit_message", queueKey(sessionID))
	res, err := s.client.Eval(ctx, admitScript, []string{sessionRunKey(sessionID), queueKey(sessionID)},
		entry.RunID, maxDepth, payload, int(run.SessionClaimTTL/time.Second)).Result()
	if err != nil {
		return run.AdmitResult{}, fmt.Errorf("runstore: admit message: %w", err)
	}
	parts, ok := res.([]any)
	if !ok || len(parts) < 2 {
		return run.AdmitResult{}, fmt.Errorf("runstore: admit message: unexpected script reply %v", res)
	}
	switch parts[0] {
	case "claimed":
		return run.AdmitResult{Outcome: run.AdmitClaimed}, nil
	case "queued":
		pos, _ := parts[1].(int64)
		return run.AdmitResult{Outcome: run.AdmitQueued, Position: int(pos)}, nil
	default:
		return run.AdmitResult{Outcome: run.AdmitFull}, nil
	}
}

// PumpHead implements run.Store: peek the head (LRANGE 0 0), then pop+claim
// it atomically under the peeked entry's byte identity.
func (s *RunStore) PumpHead(ctx context.Context, sessionID string) (run.QueueEntry, bool, error) {
	logCmd(ctx, "run.pump_head", queueKey(sessionID))
	head, err := s.client.LRange(ctx, queueKey(sessionID), 0, 0).Result()
	if err != nil {
		return run.QueueEntry{}, false, fmt.Errorf("runstore: pump head peek: %w", err)
	}
	if len(head) == 0 {
		return run.QueueEntry{}, false, nil
	}
	payload := head[0]
	var entry run.QueueEntry
	if err := json.Unmarshal([]byte(payload), &entry); err != nil {
		return run.QueueEntry{}, false, fmt.Errorf("runstore: pump head decode: %w", err)
	}
	n, err := s.client.Eval(ctx, pumpHeadScript, []string{sessionRunKey(sessionID), queueKey(sessionID)},
		payload, entry.RunID, int(run.SessionClaimTTL/time.Second)).Int()
	if err != nil {
		return run.QueueEntry{}, false, fmt.Errorf("runstore: pump head: %w", err)
	}
	if n != 1 {
		// Empty (0) or raced (−1): nothing was popped, the head stays.
		return run.QueueEntry{}, false, nil
	}
	return entry, true, nil
}

// DequeueMessage implements run.Store: locate the entry by run id over the
// (depth-bounded) list, then LREM the exact stored payload. The claim is
// untouched — a queued run never held it.
func (s *RunStore) DequeueMessage(ctx context.Context, sessionID, runID string) (run.QueueEntry, bool, error) {
	logCmd(ctx, "run.dequeue_message", queueKey(sessionID))
	raw, err := s.client.LRange(ctx, queueKey(sessionID), 0, -1).Result()
	if err != nil {
		return run.QueueEntry{}, false, fmt.Errorf("runstore: dequeue message: %w", err)
	}
	for _, payload := range raw {
		var entry run.QueueEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue // mirror entriesFromMessages: a malformed entry must not break the scan
		}
		if entry.RunID == runID {
			if err := s.client.LRem(ctx, queueKey(sessionID), 1, payload).Err(); err != nil {
				return run.QueueEntry{}, false, fmt.Errorf("runstore: dequeue message: %w", err)
			}
			return entry, true, nil
		}
	}
	return run.QueueEntry{}, false, nil
}

// QueueLen implements run.Store.
func (s *RunStore) QueueLen(ctx context.Context, sessionID string) (int, error) {
	logCmd(ctx, "run.queue_len", queueKey(sessionID))
	n, err := s.client.LLen(ctx, queueKey(sessionID)).Result()
	if err != nil {
		return 0, fmt.Errorf("runstore: queue len: %w", err)
	}
	return int(n), nil
}

// QueueEntries implements run.Store (enqueue order; malformed entries are
// skipped, not fatal).
func (s *RunStore) QueueEntries(ctx context.Context, sessionID string) ([]run.QueueEntry, error) {
	logCmd(ctx, "run.queue_entries", queueKey(sessionID))
	raw, err := s.client.LRange(ctx, queueKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("runstore: queue entries: %w", err)
	}
	var out []run.QueueEntry
	for _, payload := range raw {
		var entry run.QueueEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// TouchQueued implements run.Store: re-arm the events/meta TTL pair with the
// jittered backstop (EXPIRE on a missing key is a no-op, so a run whose keys
// already expired simply stays expired — the pump re-creates them).
func (s *RunStore) TouchQueued(ctx context.Context, runID string) error {
	logCmd(ctx, "run.touch_queued", runEventsKey(runID))
	pipe := s.client.Pipeline()
	pipe.Expire(ctx, runEventsKey(runID), run.RunKeyTTLWithJitter())
	pipe.Expire(ctx, runMetaKey(runID), run.RunKeyTTLWithJitter())
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("runstore: touch queued: %w", err)
	}
	return nil
}

// ClearQueue implements run.Store.
func (s *RunStore) ClearQueue(ctx context.Context, sessionID string) error {
	logCmd(ctx, "run.clear_queue", queueKey(sessionID))
	if err := s.client.Del(ctx, queueKey(sessionID)).Err(); err != nil {
		return fmt.Errorf("runstore: clear queue: %w", err)
	}
	return nil
}

// QueuedSessions implements run.Store: SCAN over the queue key family. Redis
// auto-deletes an empty list, so every match is a session with pending work
// (barring a key racing towards deletion, which the pump's atomic pop makes
// harmless).
func (s *RunStore) QueuedSessions(ctx context.Context) ([]string, error) {
	var out []string
	iter := s.client.Scan(ctx, 0, "queue:*", 100).Iterator()
	for iter.Next(ctx) {
		sid := strings.TrimPrefix(iter.Val(), "queue:")
		if sid != "" {
			out = append(out, sid)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("runstore: queued sessions scan: %w", err)
	}
	return out, nil
}
