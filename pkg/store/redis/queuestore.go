package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/stream"
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
//	otherwise LLEN < maxDepth   → RPUSH ARGV[3] (the entry JSON) AND — in
//	                               the same atomic step — initialize the
//	                               run's meta (HSET session_id/user_id/
//	                               status=queued/model/created_at + TTL) and
//	                               append the `queued` event to the run log
//	                               (XADD + TTL), then report the depth
//	otherwise                   → full
//
// The FIFO seal: the claim-vs-queue decision and the RPUSH are one atomic
// step, so a message arriving in the release→pump window sees the queue
// non-empty and cannot jump the line by claiming directly. The publication
// seal (review fix): the entry, its meta, and its queued event become
// visible together — a pump that pops the entry the instant it is queued can
// never see the initial meta state written afterwards (which could overwrite
// running or even done). ARGV[10] is the prebuilt queued-event JSON frame
// with the position substituted inside the script (the position is only
// known after the RPUSH); its wire shape is pinned by
// TestRunStore_AdmitPublishesQueuedState against stream.QueuedEvent.
const admitScript = `if not redis.call("GET", KEYS[1]) and redis.call("LLEN", KEYS[2]) == 0 then
	redis.call("SET", KEYS[1], ARGV[1], "EX", tonumber(ARGV[4]))
	return {"claimed", 0}
end
if redis.call("LLEN", KEYS[2]) < tonumber(ARGV[2]) then
	redis.call("RPUSH", KEYS[2], ARGV[3])
	local pos = redis.call("LLEN", KEYS[2])
	redis.call("HSET", KEYS[3],
		"session_id", ARGV[6],
		"user_id", ARGV[7],
		"status", "queued",
		"model", ARGV[8],
		"created_at", ARGV[9])
	redis.call("EXPIRE", KEYS[3], tonumber(ARGV[5]))
	-- gsub returns (string, count); bind it so the count does not expand
	-- into XADD's argument list. The substitution includes the placeholder's
	-- JSON quotes so the position lands as a NUMBER, matching the
	-- stream.QueuedEvent wire shape (pinned by test).
	local ev = string.gsub(ARGV[10], '"__POS__"', tostring(pos))
	redis.call("XADD", KEYS[4], "*", "e", ev)
	redis.call("EXPIRE", KEYS[4], tonumber(ARGV[5]))
	return {"queued", pos}
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

// queueEventPlaceholder is the admission-time depth marker inside the
// prebuilt queued-event frame; the admit script substitutes it with the
// actual post-RPUSH depth (which only it can know).
const queueEventPlaceholder = "__POS__"

// AdmitMessage implements run.Store.
func (s *RunStore) AdmitMessage(ctx context.Context, sessionID string, entry run.QueueEntry, maxDepth int) (run.AdmitResult, error) {
	payload, err := json.Marshal(entry)
	if err != nil {
		return run.AdmitResult{}, fmt.Errorf("runstore: marshal queue entry: %w", err)
	}
	// The queued event frame with the position placeholder; the shape is the
	// stream.QueuedEvent shape (pinned by test), so decoding it on the read
	// side yields the same event the hub path would have produced.
	frame, err := json.Marshal(stream.StreamEvent{
		Type: stream.EventQueued,
		Meta: map[string]any{
			stream.MetaRunID:         entry.RunID,
			stream.MetaQueuePosition: queueEventPlaceholder,
		},
	})
	if err != nil {
		return run.AdmitResult{}, fmt.Errorf("runstore: marshal queued event: %w", err)
	}
	runTTL := int(run.RunKeyTTLWithJitter().Round(time.Second).Seconds())
	logCmd(ctx, "run.admit_message", queueKey(sessionID))
	res, err := s.client.Eval(ctx, admitScript,
		[]string{sessionRunKey(sessionID), queueKey(sessionID), runMetaKey(entry.RunID), runEventsKey(entry.RunID)},
		entry.RunID, maxDepth, payload, int(run.SessionClaimTTL/time.Second), runTTL,
		entry.SessionID, entry.UserID, entry.Model, entry.EnqueuedAt.Format(time.RFC3339), string(frame)).Result()
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

// dequeueScript removes exactly one occurrence of the payload and returns
// the removal count. Doing the LREM inside the script makes the
// "entry was still listed when we scanned it" observation and the removal
// atomic: if a pump popped the entry between the Go-side scan and this
// script, the LREM removes nothing and the caller must NOT report a
// successful dequeue (review fix — a cancelled-and-actually-started run).
const dequeueScript = `return redis.call("LREM", KEYS[1], 1, ARGV[1])`

// requeueHeadScript atomically returns one popped entry to the queue front
// and releases the session claim when it still points at that entry's run.
// The entry is requeued unconditionally (never dropped); the claim release
// is compare-and-delete so a claim that already moved on (expired, cleared,
// or reclaimed by a newer run) is left alone.
const requeueHeadScript = `redis.call("LPUSH", KEYS[1], ARGV[1])
if redis.call("GET", KEYS[2]) == ARGV[2] then
	redis.call("DEL", KEYS[2])
	return 1
end
return 0`

// RequeueHead implements run.Store (shutdown abort of a popped-not-started
// entry: front-of-queue return + own-claim release in one atomic step).
func (s *RunStore) RequeueHead(ctx context.Context, sessionID string, entry run.QueueEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("runstore: requeue head: marshal: %w", err)
	}
	logCmd(ctx, "run.requeue_head", queueKey(sessionID))
	if _, err := s.client.Eval(ctx, requeueHeadScript,
		[]string{queueKey(sessionID), sessionRunKey(sessionID)},
		payload, entry.RunID).Result(); err != nil {
		return fmt.Errorf("runstore: requeue head: %w", err)
	}
	return nil
}

// DequeueMessage implements run.Store: locate the entry by run id over the
// (depth-bounded) list, then remove it atomically by exact payload. ok=false
// covers both "never queued" and "a pump took it between the scan and the
// removal" — the caller answers from the CURRENT run state either way. The
// claim is untouched — a queued run never held it.
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
			n, err := s.client.Eval(ctx, dequeueScript, []string{queueKey(sessionID)}, payload).Int()
			if err != nil {
				return run.QueueEntry{}, false, fmt.Errorf("runstore: dequeue message: %w", err)
			}
			if n == 0 {
				// The entry left the queue between the scan and the LREM —
				// a pump claimed it; the dequeue did not happen.
				return run.QueueEntry{}, false, nil
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

// retireQueuedScript terminates one dropped queued run — but only if its meta
// still exists (a long-waited entry whose key family expired stays expired;
// HSET-ing a status-only stub would break the ownership fields). Sets
// cancelled, drops the heartbeat/cancel keys, and moves meta/events to the
// terminal retain window so subscribers tailing the run close promptly
// (review fix: the clear previously left them waiting out the full TTL).
const retireQueuedScript = `if redis.call("EXISTS", KEYS[1]) == 1 then
	redis.call("HSET", KEYS[1], "status", ARGV[1])
	redis.call("DEL", KEYS[2], KEYS[3])
	redis.call("EXPIRE", KEYS[1], tonumber(ARGV[2]))
	redis.call("EXPIRE", KEYS[4], tonumber(ARGV[2]))
	return 1
end
return 0`

// drainQueueScript atomically snapshots AND deletes the queue (review fix,
// round 2): a POST that passed session validation can interleave with the
// clear — an entry admitted between a separate LRANGE and DEL would be
// deleted but absent from the retire snapshot, leaving its meta queued
// forever (unpumpable, uncancellable, SSE waiting out the TTL). One script
// returns exactly the entries it removed; anything admitted after it
// survives in the fresh queue and takes the pump's session-revalidation →
// finalize-as-error path.
const drainQueueScript = `local raw = redis.call("LRANGE", KEYS[1], 0, -1)
redis.call("DEL", KEYS[1])
return raw`

// ClearQueue implements run.Store: drop the queue and terminate every
// dropped entry's run (cancelled + retain window). The snapshot and the
// deletion are one atomic step; entries popped by a pump just before it are
// not included — the pump's session re-validation finalizes those as error
// on its own path.
func (s *RunStore) ClearQueue(ctx context.Context, sessionID string) error {
	logCmd(ctx, "run.clear_queue", queueKey(sessionID))
	res, err := s.client.Eval(ctx, drainQueueScript, []string{queueKey(sessionID)}).Result()
	if err != nil {
		return fmt.Errorf("runstore: clear queue: %w", err)
	}
	raw, _ := res.([]any)
	retain := int(run.RetainAfterTerminal.Round(time.Second).Seconds())
	for _, item := range raw {
		payload, ok := item.(string)
		if !ok {
			continue
		}
		var entry run.QueueEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue // malformed entries still left the queue with the DEL
		}
		logCmd(ctx, "run.clear_queue_run", runMetaKey(entry.RunID))
		if _, err := s.client.Eval(ctx, retireQueuedScript,
			[]string{runMetaKey(entry.RunID), runAliveKey(entry.RunID), runCancelKey(entry.RunID), runEventsKey(entry.RunID)},
			run.StatusCancelled, retain).Result(); err != nil {
			// Best-effort per run: one failing retirement must not abort the
			// rest of the sweep (the run's own TTL remains the backstop).
			return fmt.Errorf("runstore: clear queue retire %s: %w", entry.RunID, err)
		}
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
