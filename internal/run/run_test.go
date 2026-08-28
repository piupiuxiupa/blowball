package run

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lush/blowball/internal/stream"
)

func TestRegistry_RegisterCancelUnregister(t *testing.T) {
	reg := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	rr := reg.Register("run-1", cancel)

	if _, ok := reg.Get("run-1"); !ok {
		t.Fatal("Get(after Register) = missing")
	}
	select {
	case <-ctx.Done():
		t.Fatal("cancel fired before Cancel()")
	default:
	}
	if !reg.Cancel("run-1") {
		t.Fatal("Cancel(registered) = false")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Cancel did not cancel the turn context")
	}
	// Repeated cancel of a registered run is a harmless no-op-true.
	if !reg.Cancel("run-1") {
		t.Fatal("Cancel(second) = false for a still-registered run")
	}

	reg.Unregister("run-1")
	if reg.Cancel("run-1") {
		t.Fatal("Cancel(after Unregister) = true, want false")
	}
	if _, ok := reg.Get("run-1"); ok {
		t.Fatal("Get(after Unregister) = present")
	}
	// Finish is idempotent and the Done channel stays readable.
	rr.Finish()
	rr.Finish()
	select {
	case <-rr.Done():
	default:
		t.Fatal("Done not closed after Finish")
	}
}

func TestRegistry_CancelAllAndWaitAll(t *testing.T) {
	reg := NewRegistry()
	var cancels sync.WaitGroup
	cancels.Add(2)
	started := sync.WaitGroup{}
	started.Add(2)
	for _, id := range []string{"run-a", "run-b"} {
		ctx, cancel := context.WithCancel(context.Background())
		rr := reg.Register(id, cancel)
		go func(ctx context.Context, id string, rr *Run) {
			// Same LIFO teardown as the production turn goroutine: Finish
			// (unblocks WaitAll) fires before Unregister removes the entry.
			defer reg.Unregister(id)
			defer rr.Finish()
			defer cancels.Done()
			started.Done()
			<-ctx.Done() // the "turn": runs until cancelled
		}(ctx, id, rr)
	}
	started.Wait()

	if n := reg.CancelAll(); n != 2 {
		t.Fatalf("CancelAll = %d, want 2", n)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	if err := reg.WaitAll(wctx); err != nil {
		t.Fatalf("WaitAll: %v", err)
	}
	cancels.Wait()
}

func TestRegistry_WaitAllRespectsDeadline(t *testing.T) {
	reg := NewRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Register("stuck", func() {}) // never actually cancels anything
	defer reg.Unregister("stuck")

	ctx, ctxCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer ctxCancel()
	if err := reg.WaitAll(ctx); err == nil {
		t.Fatal("WaitAll(un cancellable run) = nil, want deadline error")
	}
}

func TestRunLifecycleTTLConstantsAndJitter(t *testing.T) {
	if SessionClaimTTL != 30*time.Minute {
		t.Fatalf("SessionClaimTTL = %s, want 30m", SessionClaimTTL)
	}
	if RunKeyTTL != time.Hour {
		t.Fatalf("RunKeyTTL = %s, want 1h", RunKeyTTL)
	}

	const (
		minTTL = 54 * time.Minute
		maxTTL = 66 * time.Minute
	)
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 256; i++ {
		got := RunKeyTTLWithJitter()
		if got < minTTL || got > maxTTL {
			t.Fatalf("RunKeyTTLWithJitter sample %d = %s, want [%s,%s]", i, got, minTTL, maxTTL)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("RunKeyTTLWithJitter samples were not dispersed")
	}
}

func TestMemStore_ClaimReleaseSemantics(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	if _, ok, _ := s.ClaimSession(ctx, "sess", "run-a"); !ok {
		t.Fatal("first claim failed")
	}
	if holder, ok, _ := s.ClaimSession(ctx, "sess", "run-b"); ok || holder != "run-a" {
		t.Fatalf("second claim = (%q,%v), want (run-a,false)", holder, ok)
	}
	_ = s.ReleaseSession(ctx, "sess", "run-b") // non-holder: no-op
	if holder, ok, _ := s.ClaimSession(ctx, "sess", "run-c"); ok || holder != "run-a" {
		t.Fatalf("non-holder release unlocked: (%q,%v)", holder, ok)
	}
	_ = s.ReleaseSession(ctx, "sess", "run-a")
	if _, ok, _ := s.ClaimSession(ctx, "sess", "run-d"); !ok {
		t.Fatal("holder release did not unlock")
	}
}

func TestMemStore_TTLExpiry(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	now := time.Now()
	s.Now = func() time.Time { return now }

	if _, ok, err := s.ClaimSession(ctx, "s", "r"); err != nil || !ok {
		t.Fatalf("initial claim = (%v,%v), want true,nil", err, ok)
	}
	_ = s.InitMeta(ctx, "r", RunMeta{SessionID: "s", Status: StatusRunning})
	_ = s.AppendEvent(ctx, "r", stream.StreamEvent{Type: stream.EventToken, Content: "x"})
	_ = s.Heartbeat(ctx, "r", "s")

	alive, _ := s.Alive(ctx, "r")
	if !alive {
		t.Fatal("alive before TTL = false")
	}

	// Advance past every TTL: run keys are gone, the session claim expired.
	now = now.Add(RunKeyTTL + HeartbeatTTL + time.Second)
	if _, ok, _ := s.GetMeta(ctx, "r"); ok {
		t.Fatal("meta survived RunKeyTTL")
	}
	if got, _ := s.ReadEvents(ctx, "r", "", 0); len(got) != 0 {
		t.Fatal("events survived RunKeyTTL")
	}
	if alive, _ := s.Alive(ctx, "r"); alive {
		t.Fatal("alive survived HeartbeatTTL")
	}
	if _, ok, _ := s.ClaimSession(ctx, "s", "run-next"); !ok {
		t.Fatal("session claim survived SessionClaimTTL")
	}
}

func TestMemStore_HeartbeatRearmsSessionClaim(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	now := time.Now()
	s.Now = func() time.Time { return now }

	if _, ok, _ := s.ClaimSession(ctx, "sess", "run-a"); !ok {
		t.Fatal("initial claim failed")
	}
	now = now.Add(20 * time.Minute)
	if err := s.Heartbeat(ctx, "run-a", "sess"); err != nil {
		t.Fatalf("Heartbeat(holder): %v", err)
	}
	want := now.Add(SessionClaimTTL)
	if got := s.claimExp["sess"]; !got.Equal(want) {
		t.Fatalf("claim expiry after holder heartbeat = %s, want %s", got, want)
	}

	_ = s.ReleaseSession(ctx, "sess", "run-a")
	if _, ok, _ := s.ClaimSession(ctx, "sess", "run-b"); !ok {
		t.Fatal("claiming after release failed")
	}
	expiry := s.claimExp["sess"]
	if err := s.Heartbeat(ctx, "run-a", "sess"); err != nil {
		t.Fatalf("Heartbeat(non-holder): %v", err)
	}
	if got := s.claimExp["sess"]; !got.Equal(expiry) {
		t.Fatalf("non-holder heartbeat changed claim expiry: got %s, want %s", got, expiry)
	}
}

func TestMemStore_CursorReplay(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	for _, c := range []string{"a", "b", "c"} {
		_ = s.AppendEvent(ctx, "r", stream.StreamEvent{Type: stream.EventToken, Content: c})
	}
	all, _ := s.ReadEvents(ctx, "r", "", 0)
	if len(all) != 3 {
		t.Fatalf("replay = %d events, want 3", len(all))
	}
	tail, _ := s.ReadEvents(ctx, "r", all[0].ID, 0)
	if len(tail) != 2 || tail[0].Event.Content != "b" || tail[1].Event.Content != "c" {
		t.Fatalf("tail contents = %+v, want b,c", tail)
	}
}

func TestDrainer_AppendsAllEventsAndStampsRunID(t *testing.T) {
	store := NewMemStore()
	reg := NewRegistry()
	hub := stream.NewHub(16)

	drained := StartDrainer(hub, store, reg, "run-1", "sess-1", time.Hour /* tick never fires */)

	hub.Send(stream.StreamEvent{Type: stream.EventAgentStart, Agent: "Confucius"})
	hub.Send(stream.StreamEvent{Type: stream.EventToken, Agent: "Confucius", Content: "hi"})
	hub.Send(stream.StreamEvent{Type: stream.EventDone, Meta: map[string]any{"usage": "x"}})
	hub.Close()

	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not exit after hub close")
	}

	entries, _ := store.ReadEvents(context.Background(), "run-1", "", 0)
	if len(entries) != 3 {
		t.Fatalf("log entries = %d, want 3", len(entries))
	}
	for _, ent := range entries {
		if ent.Event.Meta[MetaRunID] != "run-1" {
			t.Fatalf("entry %s missing run_id stamp: %+v", ent.ID, ent.Event.Meta)
		}
	}
	if entries[2].Event.Type != stream.EventDone {
		t.Fatalf("done event missing from log: %+v", entries)
	}
	// The drainer heartbeats immediately, so the run reads alive.
	if alive, _ := store.Alive(context.Background(), "run-1"); !alive {
		t.Fatal("run not alive after drainer start")
	}
}

func TestDrainer_ConsumesCancelFlag(t *testing.T) {
	store := NewMemStore()
	reg := NewRegistry()
	hub := stream.NewHub(4)
	tctx, tcancel := context.WithCancel(context.Background())
	reg.Register("run-1", tcancel)

	// Tick fast so the flag is observed quickly.
	drained := StartDrainer(hub, store, reg, "run-1", "sess-1", 10*time.Millisecond)
	defer func() { hub.Close(); <-drained }()

	_ = store.SetCancelFlag(context.Background(), "run-1")
	select {
	case <-tctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel flag never cancelled the local turn")
	}
	// The flag was consumed (read-and-clear).
	if flagged, _ := store.TakeCancelFlag(context.Background(), "run-1"); flagged {
		t.Fatal("cancel flag not consumed")
	}
}
