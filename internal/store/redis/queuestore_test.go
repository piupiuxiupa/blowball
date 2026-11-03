package redis

import (
	"context"
	"testing"
	"time"

	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

// redisQueueEntry builds a full entry so the JSON round-trip through the
// LIST is exercised (selection, attachments, token, timestamps).
func redisQueueEntry(runID string) run.QueueEntry {
	return run.QueueEntry{
		RunID:     runID,
		SessionID: "sess-1",
		UserID:    "u1",
		Content:   "msg " + runID,
		Model:     "gpt-test",
		Selection: run.QueueSelection{
			Model: "gpt-test", Thinking: true, Vision: true, Effort: "low",
			MaxContextTokens: 128000, MaxCompletionTokens: 8192,
			LengthExpandStep: 8192, LengthMaxRetries: 3,
		},
		EnqueuedAt: time.Unix(1700000000, 0).UTC(),
	}
}

func TestRunStore_AdmitMessageFIFOSealed(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()

	// Fast path: free claim + empty queue → claimed, claim key set with TTL.
	res, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-a"), 3)
	if err != nil || res.Outcome != run.AdmitClaimed {
		t.Fatalf("admit a = (%+v,%v), want AdmitClaimed", res, err)
	}
	if ttl := mr.TTL("session:sess-1:run"); ttl != run.SessionClaimTTL {
		t.Fatalf("claim TTL = %s, want %s", ttl, run.SessionClaimTTL)
	}
	if got, _ := mr.Get("session:sess-1:run"); got != "run-a" {
		t.Fatalf("claim holder = %q, want run-a", got)
	}

	// Held claim → queued at position 1.
	if res, err = rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-b"), 3); err != nil || res.Outcome != run.AdmitQueued || res.Position != 1 {
		t.Fatalf("admit b = (%+v,%v), want AdmitQueued pos 1", res, err)
	}
	// A NEW message while the queue is non-empty must enqueue even if the
	// claim is free (release run-a but keep the queue): the FIFO invariant
	// forbids jumping the queue by claiming directly.
	if err := rs.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if res, err = rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-c"), 3); err != nil || res.Outcome != run.AdmitQueued || res.Position != 2 {
		t.Fatalf("admit c = (%+v,%v), want AdmitQueued pos 2 (non-empty queue blocks the direct claim)", res, err)
	}
	if got, err := mr.Get("session:sess-1:run"); err == nil && got != "" {
		t.Fatalf("admit c claimed the session (%q) despite a non-empty queue", got)
	}

	// Depth cap.
	if res, err = rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-d"), 3); err != nil || res.Outcome != run.AdmitQueued {
		t.Fatalf("admit d = (%+v,%v), want AdmitQueued", res, err)
	}
	if res, err = rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-e"), 3); err != nil || res.Outcome != run.AdmitFull {
		t.Fatalf("admit e = (%+v,%v), want AdmitFull", res, err)
	}
	if n, err := rs.QueueLen(ctx, "sess-1"); err != nil || n != 3 {
		t.Fatalf("QueueLen = (%d,%v), want 3", n, err)
	}
}

func TestRunStore_PumpHeadClaimTransfer(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()

	// run-a running; b, c queued (via the queue so entries exercise the JSON
	// round-trip).
	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-b", "run-c"} {
		if _, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}

	// Held claim: the pump must not pop anything.
	if e, ok, err := rs.PumpHead(ctx, "sess-1"); err != nil || ok || e.RunID != "" {
		t.Fatalf("pump with held claim = (%q,%v,%v), want (,false,nil)", e.RunID, ok, err)
	}

	// Release → FIFO order, each pump transferring the claim with TTL.
	if err := rs.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, want := range []string{"run-b", "run-c"} {
		e, ok, err := rs.PumpHead(ctx, "sess-1")
		if err != nil || !ok || e.RunID != want {
			t.Fatalf("pump = (%q,%v,%v), want (%q,true)", e.RunID, ok, err, want)
		}
		if e.SessionID != "sess-1" || e.Content != "msg "+want || e.Model != "gpt-test" || e.Selection.MaxContextTokens != 128000 {
			t.Fatalf("pumped entry did not round-trip: %+v", e)
		}
		if got, _ := mr.Get("session:sess-1:run"); got != want {
			t.Fatalf("claim holder after pumping %s = %q, want %q", want, got, want)
		}
		if ttl := mr.TTL("session:sess-1:run"); ttl != run.SessionClaimTTL {
			t.Fatalf("claim TTL after pump = %s, want %s", ttl, run.SessionClaimTTL)
		}
		// The pumped run owns the slot; the next pump needs its release.
		if err := rs.ReleaseSession(ctx, "sess-1", want); err != nil {
			t.Fatalf("release %s: %v", want, err)
		}
	}

	// Empty queue (and the auto-deleted list key): ok=false.
	if e, ok, err := rs.PumpHead(ctx, "sess-1"); err != nil || ok || e.RunID != "" {
		t.Fatalf("pump on empty queue = (%q,%v,%v), want (,false,nil)", e.RunID, ok, err)
	}
	if mr.Exists("queue:sess-1") {
		t.Fatal("empty queue list key still exists, want Redis auto-delete")
	}
}

func TestRunStore_DequeueAndQueueEntries(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-b", "run-c", "run-d"} {
		if _, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}

	// Dequeue the middle entry; order of the rest is preserved.
	e, ok, err := rs.DequeueMessage(ctx, "sess-1", "run-c")
	if err != nil || !ok || e.RunID != "run-c" {
		t.Fatalf("dequeue c = (%q,%v,%v), want (run-c,true)", e.RunID, ok, err)
	}
	entries, err := rs.QueueEntries(ctx, "sess-1")
	if err != nil || len(entries) != 2 || entries[0].RunID != "run-b" || entries[1].RunID != "run-d" {
		t.Fatalf("queue after dequeue = (%v,%v), want [run-b run-d]", entries, err)
	}
	// Unknown id → ok=false; the claim is untouched either way.
	if _, ok, _ := rs.DequeueMessage(ctx, "sess-1", "run-zzz"); ok {
		t.Fatal("dequeue of unknown run reported ok")
	}
	if got := testGetClaim(rs, "sess-1"); got != "run-a" {
		t.Fatalf("claim holder = %q, want run-a (dequeue never touches the claim)", got)
	}

	// ClearQueue drops everything; the claim survives (session deletion
	// releases it through its own path).
	if err := rs.ClearQueue(ctx, "sess-1"); err != nil {
		t.Fatalf("clear queue: %v", err)
	}
	if n, _ := rs.QueueLen(ctx, "sess-1"); n != 0 {
		t.Fatalf("QueueLen after clear = %d, want 0", n)
	}
	if got := testGetClaim(rs, "sess-1"); got != "run-a" {
		t.Fatalf("clear queue disturbed the claim: %q", got)
	}
}

// testGetClaim reads the raw claim value via the shared client.
func testGetClaim(rs *RunStore, sid string) string {
	v, err := rs.client.Get(context.Background(), sessionRunKey(sid)).Result()
	if err != nil {
		return ""
	}
	return v
}

func TestRunStore_TouchQueuedRearmsTTL(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()

	// A queued run's key family (meta created at enqueue, events created by
	// the queued event append) gets its TTL re-armed by TouchQueued.
	if err := rs.InitMeta(ctx, "run-q", run.RunMeta{SessionID: "sess-1", Status: run.StatusQueued}); err != nil {
		t.Fatalf("init meta: %v", err)
	}
	if err := rs.AppendEvent(ctx, "run-q", stream.TokenEvent("Confucius", "x")); err != nil {
		t.Fatalf("append: %v", err)
	}
	mr.FastForward(70 * time.Minute) // past the 54–66min jittered window
	if mr.Exists("run:run-q:meta") {
		t.Fatal("meta should have expired before the touch")
	}
	_ = rs.TouchQueued(ctx, "run-q") // no-op on missing keys — must not error

	// Fresh keys ARE re-armed.
	if err := rs.InitMeta(ctx, "run-q2", run.RunMeta{SessionID: "sess-1", Status: run.StatusQueued}); err != nil {
		t.Fatalf("init meta q2: %v", err)
	}
	mr.FastForward(10 * time.Minute) // well inside the 54–66min window
	if err := rs.TouchQueued(ctx, "run-q2"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if ttl := mr.TTL("run:run-q2:meta"); ttl < 40*time.Minute {
		t.Fatalf("meta TTL after touch = %s, want re-armed toward 1h", ttl)
	}
}

// TestRunStore_AdmitPublishesQueuedState pins the atomic publication (review
// fix 3): a queued admission creates the run meta (full ownership fields,
// status queued) and appends the queued event in the SAME script as the
// RPUSH — and the appended event decodes to exactly stream.QueuedEvent's
// shape, pinning the Lua-built frame against the Go constructor (if either
// side drifts, this fails).
func TestRunStore_AdmitPublishesQueuedState(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	// Hold the claim so admission queues.
	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	entry := redisQueueEntry("run-q")
	res, err := rs.AdmitMessage(ctx, "sess-1", entry, 8)
	if err != nil || res.Outcome != run.AdmitQueued {
		t.Fatalf("admit = (%+v,%v), want AdmitQueued", res, err)
	}

	// Meta: published with the entry, status queued, full fields.
	meta, ok, err := rs.GetMeta(ctx, "run-q")
	if err != nil || !ok {
		t.Fatalf("meta after admit = (%+v,%v,%v), want exists", meta, ok, err)
	}
	if meta.Status != run.StatusQueued || meta.SessionID != "sess-1" ||
		meta.UserID != "u1" || meta.Model != "gpt-test" {
		t.Fatalf("meta = %+v, want full queued meta", meta)
	}

	// Event log: exactly one queued event, byte-shape-equal to the Go
	// constructor's output for the admission position.
	entries, err := rs.ReadEvents(ctx, "run-q", "0", 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("events = (%v,%v), want exactly the queued event", entries, err)
	}
	got := entries[0].Event
	want := stream.QueuedEvent("run-q", res.Position)
	if got.Type != want.Type || got.Agent != want.Agent || got.Content != want.Content {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
	if got.Meta[stream.MetaRunID] != "run-q" {
		t.Fatalf("event run_id = %v, want run-q", got.Meta[stream.MetaRunID])
	}
	if pos, _ := got.Meta[stream.MetaQueuePosition].(float64); int(pos) != res.Position {
		t.Fatalf("event queue_position = %v, want %d", got.Meta[stream.MetaQueuePosition], res.Position)
	}
}

// TestRunStore_DequeueAfterPumpMisses: once the pump has popped the entry,
// the dequeue reports ok=false — the cancel endpoint must answer from the
// run's current state instead of claiming a cancellation that never
// happened (review fix 2: the LREM count, not just its error, decides).
func TestRunStore_DequeueAfterPumpMisses(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-b"), 8); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := rs.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, ok, err := rs.PumpHead(ctx, "sess-1"); err != nil || !ok {
		t.Fatalf("pump = (%v,%v), want ok", ok, err)
	}
	// The pump took the entry: a dequeue attempt for the same run must NOT
	// report success (it did not remove anything).
	if _, ok, err := rs.DequeueMessage(ctx, "sess-1", "run-b"); err != nil || ok {
		t.Fatalf("dequeue after pump = ok=%v err=%v, want ok=false (nothing removed)", ok, err)
	}
}

// TestRunStore_ClearQueueTerminatesQueuedRuns (review fix 6): clearing the
// queue retires every dropped run as cancelled with the retain window, so
// tailing subscribers observe a terminal status and close.
func TestRunStore_ClearQueueTerminatesQueuedRuns(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-b", "run-c"} {
		if _, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	if err := rs.ClearQueue(ctx, "sess-1"); err != nil {
		t.Fatalf("clear queue: %v", err)
	}
	if n, _ := rs.QueueLen(ctx, "sess-1"); n != 0 {
		t.Fatalf("QueueLen after clear = %d, want 0", n)
	}
	for _, id := range []string{"run-b", "run-c"} {
		meta, ok, err := rs.GetMeta(ctx, id)
		if err != nil || !ok {
			t.Fatalf("meta %s = (%+v,%v,%v), want exists (cancelled, full fields)", meta, ok, err, id)
		}
		if meta.Status != run.StatusCancelled {
			t.Fatalf("run %s status = %q, want cancelled", id, meta.Status)
		}
		// Ownership fields survive the retirement (no status-only stub).
		if meta.SessionID != "sess-1" || meta.UserID != "u1" {
			t.Fatalf("run %s meta = %+v, want full ownership fields", id, meta)
		}
	}
}

// TestRunStore_ClearQueueDrainIsSnapshotAndDelete (review round 2, fix 2):
// the clear drains exactly the entries present at the atomic snapshot+delete
// step — an entry admitted AFTER the drain survives in the fresh queue (with
// its queued meta) and is not swept by the retire loop, which would leave an
// unretired, unpumpable, uncancellable run. The admission-after-drain entry
// takes the pump's session-revalidation path on its own.
func TestRunStore_ClearQueueDrainIsSnapshotAndDelete(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	// Two entries drained and retired...
	if _, _, err := rs.ClaimSession(ctx, "sess-1", "run-holder"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range []string{"run-a", "run-b"} {
		if _, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry(id), 8); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	if err := rs.ClearQueue(ctx, "sess-1"); err != nil {
		t.Fatalf("clear queue: %v", err)
	}
	for _, id := range []string{"run-a", "run-b"} {
		meta, ok, _ := rs.GetMeta(ctx, id)
		if !ok || meta.Status != run.StatusCancelled {
			t.Fatalf("run %s = (%+v,%v), want cancelled", id, meta, ok)
		}
	}

	// ...and one admitted AFTER the drain: it must survive the sweep —
	// queued, pumpable, cancellable — not silently deleted-and-orphaned.
	if res, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("run-late"), 8); err != nil || res.Outcome != run.AdmitQueued {
		t.Fatalf("late admit = (%+v,%v), want AdmitQueued", res, err)
	}
	if n, _ := rs.QueueLen(ctx, "sess-1"); n != 1 {
		t.Fatalf("QueueLen after late admit = %d, want 1 (the drain must not delete it)", n)
	}
	meta, ok, _ := rs.GetMeta(ctx, "run-late")
	if !ok || meta.Status != run.StatusQueued {
		t.Fatalf("late run = (%+v,%v), want queued meta", meta, ok)
	}
	// And the ordinary queued-cancel still terminates it.
	if _, ok, err := rs.DequeueMessage(ctx, "sess-1", "run-late"); err != nil || !ok {
		t.Fatalf("late dequeue = ok=%v err=%v, want ok=true (the entry survived the drain)", ok, err)
	}
	if err := rs.SetStatus(ctx, "run-late", run.StatusCancelled); err != nil {
		t.Fatalf("late set status: %v", err)
	}
}
