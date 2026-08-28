package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

// RunStore implements run.Store over Redis (turn-detach-resume capability).
// Key families and TTLs mirror the run package contract:
//
//	run:{rid}:events  Stream — one entry per turn event, MAXLEN-bounded, with
//	                  a 1h±10%-jittered crash backstop refreshed on writes
//	run:{rid}:meta    Hash   — session_id/user_id/status/model/created_at,
//	                  with the same 1h±10%-jittered backstop
//	run:{rid}:alive   String — heartbeat, run.HeartbeatTTL
//	run:{rid}:cancel  String — cross-process cancel flag (GETDEL-consumed),
//	                  with the same 1h±10%-jittered backstop
//	session:{sid}:run String — single-active-run claim (SET NX, compare-del),
//	                  30min backstop compare-and-rearmed by heartbeats
//
// compile-time interface check.
var _ run.Store = (*RunStore)(nil)

// RunStore wraps the shared *redis.Client with the run-state commands.
type RunStore struct {
	client *redis.Client
}

// RunStore returns the run-state view over the Store's shared client.
func (s *Store) RunStore() *RunStore { return &RunStore{client: s.client} }

func runEventsKey(runID string) string { return "run:" + runID + ":events" }
func runMetaKey(runID string) string   { return "run:" + runID + ":meta" }
func runAliveKey(runID string) string  { return "run:" + runID + ":alive" }
func runCancelKey(runID string) string { return "run:" + runID + ":cancel" }
func sessionRunKey(sid string) string  { return "session:" + sid + ":run" }

// releaseClaimScript compare-and-deletes the session claim so a stale
// releaser (e.g. a turn that finished after its claim already expired and was
// reclaimed) never deletes a newer run's claim.
const releaseClaimScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`

// rearmClaimScript compare-and-rearms a claim only while it still points at
// the heartbeat's run, so a stale drainer cannot extend a newer claim.
const rearmClaimScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("EXPIRE", KEYS[1], ARGV[2]) else return 0 end`

// AppendEvent implements run.Store: XADD the JSON-encoded event with an
// approximate MAXLEN bound and (re)arm the stream TTL. Entry ids are
// Redis-assigned ("<ms>-<seq>") and double as SSE ids.
func (s *RunStore) AppendEvent(ctx context.Context, runID string, e stream.StreamEvent) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("runstore: marshal event: %w", err)
	}
	logCmd(ctx, "run.append_event", runEventsKey(runID))
	pipe := s.client.Pipeline()
	xadd := pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: runEventsKey(runID),
		MaxLen: run.MaxStreamLen,
		Approx: true,
		Values: map[string]any{"e": payload},
	})
	pipe.Expire(ctx, runEventsKey(runID), run.RunKeyTTLWithJitter())
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("runstore: xadd: %w", err)
	}
	_ = xadd // entry id assigned server-side; readers learn it via ReadEvents
	return nil
}

// ReadEvents implements run.Store: XRANGE replay after the cursor (exclusive
// range start), then a blocking XREAD tail when the log has nothing new.
// A block timeout surfaces as (nil, nil) — the caller's poll loop decides
// what to do next.
func (s *RunStore) ReadEvents(ctx context.Context, runID, after string, block time.Duration) ([]run.Entry, error) {
	if after == "" {
		after = "0"
	}
	// "0" (or empty) replays from the stream's minimum id; any other cursor
	// resumes exclusively after it.
	start := "-"
	if after != "0" {
		start = "(" + after
	}
	msgs, err := s.client.XRange(ctx, runEventsKey(runID), start, "+").Result()
	if err != nil {
		return nil, fmt.Errorf("runstore: xrange: %w", err)
	}
	if len(msgs) > 0 {
		return entriesFromMessages(msgs), nil
	}
	if block > 0 {
		res, err := s.client.XRead(ctx, &redis.XReadArgs{
			Streams: []string{runEventsKey(runID), after},
			Count:   128,
			Block:   block,
		}).Result()
		if err != nil {
			if err == redis.Nil { // block timeout with no new entries
				return nil, nil
			}
			return nil, fmt.Errorf("runstore: xread: %w", err)
		}
		for _, st := range res {
			if st.Stream != runEventsKey(runID) {
				continue
			}
			return entriesFromMessages(st.Messages), nil
		}
	}
	return nil, nil
}

func entriesFromMessages(msgs []redis.XMessage) []run.Entry {
	out := make([]run.Entry, 0, len(msgs))
	for _, m := range msgs {
		raw, _ := m.Values["e"].(string)
		var e stream.StreamEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue // a malformed entry must not break the replay cursor
		}
		out = append(out, run.Entry{ID: m.ID, Event: e})
	}
	return out
}

// InitMeta implements run.Store.
func (s *RunStore) InitMeta(ctx context.Context, runID string, m run.RunMeta) error {
	key := runMetaKey(runID)
	logCmd(ctx, "run.init_meta", key)
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, key, map[string]any{
		"session_id": m.SessionID,
		"user_id":    m.UserID,
		"status":     m.Status,
		"model":      m.Model,
		"created_at": m.CreatedAt,
	})
	pipe.Expire(ctx, key, run.RunKeyTTLWithJitter())
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("runstore: init meta: %w", err)
	}
	return nil
}

// SetStatus implements run.Store.
func (s *RunStore) SetStatus(ctx context.Context, runID, status string) error {
	key := runMetaKey(runID)
	logCmd(ctx, "run.set_status", key)
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, key, "status", status)
	pipe.Expire(ctx, key, run.RunKeyTTLWithJitter())
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("runstore: set status: %w", err)
	}
	return nil
}

// GetMeta implements run.Store.
func (s *RunStore) GetMeta(ctx context.Context, runID string) (run.RunMeta, bool, error) {
	vals, err := s.client.HGetAll(ctx, runMetaKey(runID)).Result()
	if err != nil {
		return run.RunMeta{}, false, fmt.Errorf("runstore: get meta: %w", err)
	}
	if len(vals) == 0 {
		return run.RunMeta{}, false, nil
	}
	return run.RunMeta{
		SessionID: vals["session_id"],
		UserID:    vals["user_id"],
		Status:    vals["status"],
		Model:     vals["model"],
		CreatedAt: vals["created_at"],
	}, true, nil
}

// ClaimSession implements run.Store: SET NX with the session-claim TTL.
func (s *RunStore) ClaimSession(ctx context.Context, sessionID, runID string) (string, bool, error) {
	key := sessionRunKey(sessionID)
	logCmd(ctx, "run.claim_session", key)
	ok, err := s.client.SetNX(ctx, key, runID, run.SessionClaimTTL).Result()
	if err != nil {
		return "", false, fmt.Errorf("runstore: claim session: %w", err)
	}
	if ok {
		return runID, true, nil
	}
	holder, err := s.client.Get(ctx, key).Result()
	if err != nil && err != redis.Nil {
		return "", false, nil // claim held but unreadable: treat as busy
	}
	return holder, false, nil
}

// ReleaseSession implements run.Store: Lua compare-and-delete.
func (s *RunStore) ReleaseSession(ctx context.Context, sessionID, runID string) error {
	key := sessionRunKey(sessionID)
	logCmd(ctx, "run.release_session", key)
	if err := s.client.Eval(ctx, releaseClaimScript, []string{key}, runID).Err(); err != nil {
		return fmt.Errorf("runstore: release session: %w", err)
	}
	return nil
}

// ActiveRuns implements run.Store: one MGET over the session claim keys.
func (s *RunStore) ActiveRuns(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	if len(sessionIDs) == 0 {
		return map[string]string{}, nil
	}
	keys := make([]string, 0, len(sessionIDs))
	for _, sid := range sessionIDs {
		keys = append(keys, sessionRunKey(sid))
	}
	vals, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("runstore: active runs: %w", err)
	}
	out := make(map[string]string, len(sessionIDs))
	for i, sid := range sessionIDs {
		if v, ok := vals[i].(string); ok && v != "" {
			out[sid] = v
		}
	}
	return out, nil
}

// Heartbeat implements run.Store: refresh the alive TTL, re-arm the event-log
// / meta TTLs, and compare-and-rearm the session claim so a long turn never
// expires or loses mutual exclusion mid-flight.
func (s *RunStore) Heartbeat(ctx context.Context, runID, sessionID string) error {
	pipe := s.client.Pipeline()
	pipe.Set(ctx, runAliveKey(runID), runID, run.HeartbeatTTL)
	pipe.Expire(ctx, runEventsKey(runID), run.RunKeyTTLWithJitter())
	pipe.Expire(ctx, runMetaKey(runID), run.RunKeyTTLWithJitter())
	pipe.Eval(ctx, rearmClaimScript, []string{sessionRunKey(sessionID)}, runID, int(run.SessionClaimTTL/time.Second))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("runstore: heartbeat: %w", err)
	}
	return nil
}

// Alive implements run.Store.
func (s *RunStore) Alive(ctx context.Context, runID string) (bool, error) {
	n, err := s.client.Exists(ctx, runAliveKey(runID)).Result()
	if err != nil {
		return false, fmt.Errorf("runstore: alive: %w", err)
	}
	return n > 0, nil
}

// SetCancelFlag implements run.Store.
func (s *RunStore) SetCancelFlag(ctx context.Context, runID string) error {
	if err := s.client.Set(ctx, runCancelKey(runID), "1", run.RunKeyTTLWithJitter()).Err(); err != nil {
		return fmt.Errorf("runstore: set cancel flag: %w", err)
	}
	return nil
}

// TakeCancelFlag implements run.Store: atomic read-and-clear.
func (s *RunStore) TakeCancelFlag(ctx context.Context, runID string) (bool, error) {
	val, err := s.client.GetDel(ctx, runCancelKey(runID)).Result()
	if err != nil {
		if err == redis.Nil {
			return false, nil
		}
		return false, fmt.Errorf("runstore: take cancel flag: %w", err)
	}
	return val == "1", nil
}

// Retire implements run.Store: drop heartbeat/cancel now, move event log and
// meta to the retain-window TTL.
func (s *RunStore) Retire(ctx context.Context, runID string, retain time.Duration) error {
	pipe := s.client.Pipeline()
	pipe.Del(ctx, runAliveKey(runID), runCancelKey(runID))
	pipe.Expire(ctx, runEventsKey(runID), retain)
	pipe.Expire(ctx, runMetaKey(runID), retain)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("runstore: retire: %w", err)
	}
	return nil
}
