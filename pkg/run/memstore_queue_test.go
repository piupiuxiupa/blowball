package run

import (
	"context"
	"testing"
	"time"
)

// queueEntryFor builds a minimal entry for queue-semantics tests.
func queueEntryFor(runID string) QueueEntry {
	return QueueEntry{RunID: runID, SessionID: "sess-1", UserID: "u1", Content: "msg " + runID}
}

// TestMemStore_AdmitFastPathClaimsOnlyWhenIdle pins the FIFO invariant's fast
// half: a claim free AND an empty queue lets a message claim directly; a held
// claim or a non-empty queue forces RPUSH (no line jumping), and the depth cap
// reports AdmitFull.
func TestMemStore_AdmitFastPathClaimsOnlyWhenIdle(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()

	// Free claim + empty queue → claimed.
	res, err := m.AdmitMessage(ctx, "sess-1", queueEntryFor("run-a"), 3)
	if err != nil || res.Outcome != AdmitClaimed {
		t.Fatalf("admit a = (%+v,%v), want AdmitClaimed", res, err)
	}
	// Claim now held (run-a running) → queue.
	res, err = m.AdmitMessage(ctx, "sess-1", queueEntryFor("run-b"), 3)
	if err != nil || res.Outcome != AdmitQueued || res.Position != 1 {
		t.Fatalf("admit b = (%+v,%v), want AdmitQueued pos 1", res, err)
	}
	// Release the claim but keep the queue non-empty: a new message must NOT
	// claim directly (that would jump run-b) — it enqueues behind it.
	if err := m.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, err = m.AdmitMessage(ctx, "sess-1", queueEntryFor("run-c"), 3)
	if err != nil || res.Outcome != AdmitQueued || res.Position != 2 {
		t.Fatalf("admit c = (%+v,%v), want AdmitQueued pos 2 (queue non-empty blocks direct claim)", res, err)
	}

	// Depth cap: one more fits, the next is full.
	if res, err = m.AdmitMessage(ctx, "sess-1", queueEntryFor("run-d"), 3); err != nil || res.Outcome != AdmitQueued {
		t.Fatalf("admit d = (%+v,%v), want AdmitQueued", res, err)
	}
	if res, err = m.AdmitMessage(ctx, "sess-1", queueEntryFor("run-e"), 3); err != nil || res.Outcome != AdmitFull {
		t.Fatalf("admit e = (%+v,%v), want AdmitFull", res, err)
	}
	if n, _ := m.QueueLen(ctx, "sess-1"); n != 3 {
		t.Fatalf("QueueLen = %d, want 3", n)
	}
}

// TestMemStore_PumpHeadOrderAndClaimTransfer pins the pump half: the head is
// popped and the claim transferred atomically; a held claim leaves the head
// enqueued; entries come off strictly in enqueue order.
func TestMemStore_PumpHeadOrderAndClaimTransfer(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()

	// run-a running, b and c queued.
	if _, _, err := m.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-b", "run-c"} {
		if _, err := m.AdmitMessage(ctx, "sess-1", queueEntryFor(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	// A held claim blocks the pump (a racing admission won the slot).
	if _, ok, _ := m.PumpHead(ctx, "sess-1"); ok {
		t.Fatal("PumpHead succeeded while the claim was held")
	}
	// Release → pump drains in FIFO order, each pop carrying the claim; the
	// claim transfer means the next pump only succeeds after the pumped run's
	// turn finalizes and releases (simulated here by ReleaseSession).
	if err := m.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, want := range []string{"run-b", "run-c"} {
		e, ok, err := m.PumpHead(ctx, "sess-1")
		if err != nil || !ok || e.RunID != want {
			t.Fatalf("pump = (%q,%v,%v), want (%q,true)", e.RunID, ok, err, want)
		}
		if holder, ok, _ := m.ClaimSession(ctx, "sess-1", "run-x"); ok || holder != want {
			t.Fatalf("claim holder after pumping %s = (%q,%v), want holder %q held", want, holder, ok, want)
		}
		if err := m.ReleaseSession(ctx, "sess-1", want); err != nil {
			t.Fatalf("release %s: %v", want, err)
		}
	}
	// Empty queue → ok=false, no claim churn.
	if e, ok, _ := m.PumpHead(ctx, "sess-1"); ok {
		t.Fatalf("pump on empty queue = (%q,true), want false", e.RunID)
	}
}

// TestMemStore_DequeueByRunID covers the cancel-of-queued path: removal by
// run id keeps the rest in order, and an unknown id reports ok=false.
func TestMemStore_DequeueByRunID(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()

	if _, _, err := m.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-b", "run-c", "run-d"} {
		if _, err := m.AdmitMessage(ctx, "sess-1", queueEntryFor(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	e, ok, err := m.DequeueMessage(ctx, "sess-1", "run-c")
	if err != nil || !ok || e.RunID != "run-c" {
		t.Fatalf("dequeue c = (%q,%v,%v), want (run-c,true)", e.RunID, ok, err)
	}
	entries, _ := m.QueueEntries(ctx, "sess-1")
	if len(entries) != 2 || entries[0].RunID != "run-b" || entries[1].RunID != "run-d" {
		t.Fatalf("queue after dequeue = %v, want [run-b run-d]", entries)
	}
	if _, ok, _ := m.DequeueMessage(ctx, "sess-1", "run-zzz"); ok {
		t.Fatal("dequeue of unknown run reported ok")
	}
	// The claim must be untouched by a dequeue (queued runs never held it).
	if holder, _, _ := m.ClaimSession(ctx, "sess-1", "run-x2"); holder != "run-a" {
		t.Fatalf("claim holder changed by dequeue: %q", holder)
	}
}

// TestMemStore_TouchQueuedRearmsTTL covers the reconciler's TTL keep-alive:
// the run-key expiry moves forward for a queued run, and a run with no keys
// (already expired) is not resurrected.
func TestMemStore_TouchQueuedRearmsTTL(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()

	m.Now = func() time.Time { return time.Unix(1000, 0) }
	if err := m.InitMeta(ctx, "run-q", RunMeta{SessionID: "sess-1", Status: StatusQueued}); err != nil {
		t.Fatalf("init meta: %v", err)
	}

	m.Now = func() time.Time { return time.Unix(1000 + int64(RunKeyTTL.Seconds()/2), 0) }
	if err := m.TouchQueued(ctx, "run-q"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if got := m.expire["run-q"]; !got.Equal(m.now().Add(RunKeyTTL)) {
		t.Fatalf("expire = %s, want %s (re-armed from touch time)", got, m.now().Add(RunKeyTTL))
	}

	// A run whose key family expired stays expired (no resurrection). The
	// touch above re-armed expiry to touchTime + RunKeyTTL, so jump past it.
	m.Now = func() time.Time { return time.Unix(1000 + int64(RunKeyTTL.Seconds()/2) + int64(RunKeyTTL.Seconds()) + 10, 0) }
	if err := m.TouchQueued(ctx, "run-q"); err != nil {
		t.Fatalf("touch after expiry: %v", err)
	}
	if _, ok := m.expire["run-q"]; ok {
		t.Fatal("touch resurrected an expired run's key family")
	}
}
