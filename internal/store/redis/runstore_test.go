package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/stream"
)

func testRunStore(t *testing.T) (*RunStore, *miniredis.Miniredis) {
	t.Helper()
	s, mr := newTestStore(t, time.Hour)
	return s.RunStore(), mr
}

func TestRunStore_ClaimSessionMutex(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()

	// First claim wins.
	holder, ok, err := rs.ClaimSession(ctx, "sess-1", "run-a")
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("first claim = (%q,%v,%v), want (run-a,true,nil)", holder, ok, err)
	}
	if ttl := mr.TTL("session:sess-1:run"); ttl != run.SessionClaimTTL {
		t.Fatalf("claim TTL = %s, want %s", ttl, run.SessionClaimTTL)
	}
	// Second claim fails and reports the holder.
	holder, ok, err = rs.ClaimSession(ctx, "sess-1", "run-b")
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("second claim = (%q,%v,%v), want (run-a,false,nil)", holder, ok, err)
	}
	// Release by a non-holder must not unlock.
	if err := rs.ReleaseSession(ctx, "sess-1", "run-b"); err != nil {
		t.Fatalf("ReleaseSession(non-holder): %v", err)
	}
	if _, ok, _ := rs.ClaimSession(ctx, "sess-1", "run-c"); ok {
		t.Fatal("non-holder release unlocked the session")
	}
	// Release by the holder unlocks.
	if err := rs.ReleaseSession(ctx, "sess-1", "run-a"); err != nil {
		t.Fatalf("ReleaseSession(holder): %v", err)
	}
	if _, ok, _ := rs.ClaimSession(ctx, "sess-1", "run-c"); !ok {
		t.Fatal("release did not unlock the session for a new claim")
	}
}

func TestRunStore_RunKeyTTLJitter(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()

	const runs = 16
	metaTTLs := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		runID := fmt.Sprintf("run-%02d", i)
		sessionID := fmt.Sprintf("sess-%02d", i)
		meta := run.RunMeta{SessionID: sessionID, Status: run.StatusRunning}
		if err := rs.InitMeta(ctx, runID, meta); err != nil {
			t.Fatalf("InitMeta(%s): %v", runID, err)
		}
		if err := rs.AppendEvent(ctx, runID, stream.StreamEvent{Type: stream.EventToken}); err != nil {
			t.Fatalf("AppendEvent(%s): %v", runID, err)
		}
		if err := rs.SetStatus(ctx, runID, run.StatusRunning); err != nil {
			t.Fatalf("SetStatus(%s): %v", runID, err)
		}
		if err := rs.SetCancelFlag(ctx, runID); err != nil {
			t.Fatalf("SetCancelFlag(%s): %v", runID, err)
		}
		if err := rs.Heartbeat(ctx, runID, sessionID); err != nil {
			t.Fatalf("Heartbeat(%s): %v", runID, err)
		}

		for _, key := range []string{
			"run:" + runID + ":events",
			"run:" + runID + ":meta",
			"run:" + runID + ":cancel",
		} {
			assertRunKeyTTLRange(t, mr.TTL(key), key)
		}
		metaTTLs = append(metaTTLs, mr.TTL("run:"+runID+":meta"))
	}

	distinct := make(map[time.Duration]struct{})
	for _, ttl := range metaTTLs {
		distinct[ttl] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("all %d meta TTLs are %s; jitter did not disperse expiry", len(metaTTLs), metaTTLs[0])
	}
}

func assertRunKeyTTLRange(t *testing.T, ttl time.Duration, key string) {
	t.Helper()
	const (
		minTTL = 54 * time.Minute
		maxTTL = 66 * time.Minute
	)
	if ttl < minTTL || ttl > maxTTL {
		t.Fatalf("%s TTL = %s, want [%s,%s]", key, ttl, minTTL, maxTTL)
	}
}

func TestRunStore_EventLogCursorReplay(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	events := []stream.StreamEvent{
		{Type: stream.EventAgentStart, Agent: "Confucius"},
		{Type: stream.EventToken, Agent: "Confucius", Content: "hello"},
		{Type: stream.EventToken, Agent: "Confucius", Content: " world"},
		{Type: stream.EventDone},
	}
	for _, e := range events {
		if err := rs.AppendEvent(ctx, "run-1", e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Full replay from the beginning.
	all, err := rs.ReadEvents(ctx, "run-1", "", 0)
	if err != nil {
		t.Fatalf("ReadEvents(all): %v", err)
	}
	if len(all) != len(events) {
		t.Fatalf("replay got %d events, want %d", len(all), len(events))
	}
	for i, ent := range all {
		if ent.Event.Type != events[i].Type || ent.Event.Content != events[i].Content {
			t.Fatalf("replay[%d] = %+v, want %+v", i, ent.Event, events[i])
		}
	}

	// Resume strictly after the second entry: no gap, no dup.
	tail, err := rs.ReadEvents(ctx, "run-1", all[1].ID, 0)
	if err != nil {
		t.Fatalf("ReadEvents(after): %v", err)
	}
	if len(tail) != 2 || tail[0].ID != all[2].ID || tail[1].ID != all[3].ID {
		t.Fatalf("tail = %v, want entries %s..%s", ids(tail), all[2].ID, all[3].ID)
	}

	// Empty tail with block=0 returns nothing.
	if got, _ := rs.ReadEvents(ctx, "run-1", all[3].ID, 0); len(got) != 0 {
		t.Fatalf("tail after last = %v, want empty", ids(got))
	}

	// Blocking read with no new entries times out as (nil, nil).
	start := time.Now()
	got, err := rs.ReadEvents(ctx, "run-1", all[3].ID, 50*time.Millisecond)
	if err != nil || len(got) != 0 {
		t.Fatalf("blocked read = (%v,%v), want empty,nil", ids(got), err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("blocked read returned before the block window")
	}
}

func ids(entries []run.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestRunStore_MetaLifecycle(t *testing.T) {
	rs, ref := testRunStore(t)
	ctx := context.Background()

	meta := run.RunMeta{SessionID: "sess-1", UserID: "u-1", Status: run.StatusRunning, CreatedAt: "2026-08-19T00:00:00Z"}
	if err := rs.InitMeta(ctx, "run-1", meta); err != nil {
		t.Fatalf("InitMeta: %v", err)
	}
	got, ok, err := rs.GetMeta(ctx, "run-1")
	if err != nil || !ok || got != meta {
		t.Fatalf("GetMeta = (%+v,%v,%v), want (%+v,true,nil)", got, ok, err, meta)
	}
	if err := rs.SetStatus(ctx, "run-1", run.StatusDone); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if got, _, _ := rs.GetMeta(ctx, "run-1"); got.Status != run.StatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	// Unknown run reads as missing.
	if _, ok, _ := rs.GetMeta(ctx, "run-nope"); ok {
		t.Fatal("GetMeta(unknown) = found, want missing")
	}

	// Retire shortens the meta TTL to the retain window; FastForward past it
	// and the meta is gone.
	if err := rs.Retire(ctx, "run-1", run.RetainAfterTerminal); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	ref.FastForward(run.RetainAfterTerminal + time.Second)
	if _, ok, _ := rs.GetMeta(ctx, "run-1"); ok {
		t.Fatal("meta survived past the retain window")
	}
}

func TestRunStore_HeartbeatAndAlive(t *testing.T) {
	rs, ref := testRunStore(t)
	ctx := context.Background()

	alive, err := rs.Alive(ctx, "run-1")
	if err != nil || alive {
		t.Fatalf("Alive(before) = (%v,%v), want false,nil", alive, err)
	}
	if err := rs.Heartbeat(ctx, "run-1", "sess-1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if alive, _ := rs.Alive(ctx, "run-1"); !alive {
		t.Fatal("Alive(after heartbeat) = false, want true")
	}
	// The heartbeat TTL expires; the run reads as dead.
	ref.FastForward(run.HeartbeatTTL + time.Second)
	if alive, _ := rs.Alive(ctx, "run-1"); alive {
		t.Fatal("Alive(after TTL) = true, want false")
	}
}

func TestRunStore_HeartbeatRearmsSessionClaim(t *testing.T) {
	rs, mr := testRunStore(t)
	ctx := context.Background()
	claimKey := "session:sess-1:run"

	if _, ok, _ := rs.ClaimSession(ctx, "sess-1", "run-a"); !ok {
		t.Fatal("initial claim failed")
	}
	mr.FastForward(10 * time.Minute)
	if err := rs.Heartbeat(ctx, "run-a", "sess-1"); err != nil {
		t.Fatalf("Heartbeat(holder): %v", err)
	}
	if ttl := mr.TTL(claimKey); ttl != run.SessionClaimTTL {
		t.Fatalf("claim TTL after holder heartbeat = %s, want %s", ttl, run.SessionClaimTTL)
	}

	_ = rs.ReleaseSession(ctx, "sess-1", "run-a")
	if err := rs.Heartbeat(ctx, "run-a", "sess-1"); err != nil {
		t.Fatalf("Heartbeat(released): %v", err)
	}
	if _, ok, _ := rs.ClaimSession(ctx, "sess-1", "run-b"); !ok {
		t.Fatal("released claim was not available to the next run")
	}
	mr.FastForward(10 * time.Minute)
	before := mr.TTL(claimKey)
	if err := rs.Heartbeat(ctx, "run-a", "sess-1"); err != nil {
		t.Fatalf("Heartbeat(non-holder): %v", err)
	}
	if after := mr.TTL(claimKey); after != before {
		t.Fatalf("non-holder heartbeat changed claim TTL: got %s, want %s", after, before)
	}
}

func TestRunStore_CancelFlag(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	if flagged, _ := rs.TakeCancelFlag(ctx, "run-1"); flagged {
		t.Fatal("TakeCancelFlag(before set) = true, want false")
	}
	if err := rs.SetCancelFlag(ctx, "run-1"); err != nil {
		t.Fatalf("SetCancelFlag: %v", err)
	}
	if flagged, _ := rs.TakeCancelFlag(ctx, "run-1"); !flagged {
		t.Fatal("TakeCancelFlag(after set) = false, want true")
	}
	if flagged, _ := rs.TakeCancelFlag(ctx, "run-1"); flagged {
		t.Fatal("TakeCancelFlag(consuming twice) = true, want false")
	}
}

func TestRunStore_ActiveRuns(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	if _, ok, _ := rs.ClaimSession(ctx, "sess-1", "run-a"); !ok {
		t.Fatal("claim sess-1 failed")
	}
	runs, err := rs.ActiveRuns(ctx, []string{"sess-1", "sess-2"})
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if runs["sess-1"] != "run-a" || runs["sess-2"] != "" {
		t.Fatalf("ActiveRuns = %v, want sess-1=run-a sess-2=empty", runs)
	}
}
