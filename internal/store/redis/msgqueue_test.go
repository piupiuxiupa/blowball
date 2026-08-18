package redis

import (
	"context"
	"testing"
	"time"
)

// TestAppendMessagesDual_WritesBothKeys verifies the single-pipeline dual
// write: the read cache and the ingest queue receive the identical elements,
// the cache key carries the TTL while the queue key deliberately carries none.
func TestAppendMessagesDual_WritesBothKeys(t *testing.T) {
	store, mr := newTestStore(t, time.Hour)
	ctx := context.Background()

	raws := [][]byte{[]byte(`{"i":0}`), []byte(`{"i":1}`), []byte(`{"i":2}`)}
	if err := store.AppendMessagesDual(ctx, "s-1", raws); err != nil {
		t.Fatalf("AppendMessagesDual: %v", err)
	}

	// Read cache: same elements, TTL applied.
	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(raws) {
		t.Fatalf("cache len = %d, want %d", len(got), len(raws))
	}
	for i := range raws {
		if string(got[i]) != string(raws[i]) {
			t.Errorf("cache[%d] = %q, want %q", i, got[i], raws[i])
		}
	}
	if ttl := mr.TTL("msgs:s-1"); ttl <= 0 {
		t.Fatalf("cache key must carry a TTL, got %v", ttl)
	}

	// Ingest queue: same elements, NO TTL (an expiry would drop un-flushed
	// messages, which are business data).
	n, err := store.LenMessageBuffer(ctx)
	if err != nil {
		t.Fatalf("LenMessageBuffer: %v", err)
	}
	if n != int64(len(raws)) {
		t.Fatalf("buffer len = %d, want %d", n, len(raws))
	}
	if ttl := mr.TTL("msgs:buffer"); ttl != 0 {
		t.Fatalf("buffer key must NOT carry a TTL, got %v", ttl)
	}
	queued, err := store.Client().LRange(ctx, "msgs:buffer", 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE buffer: %v", err)
	}
	for i := range raws {
		if queued[i] != string(raws[i]) {
			t.Errorf("buffer[%d] = %q, want %q", i, queued[i], raws[i])
		}
	}
}

// TestAppendMessagesDual_EmptyIsNoop verifies an empty batch leaves both keys
// untouched (no phantom RPUSH, no key creation).
func TestAppendMessagesDual_EmptyIsNoop(t *testing.T) {
	store, mr := newTestStore(t, time.Hour)
	ctx := context.Background()

	if err := store.AppendMessagesDual(ctx, "s-1", nil); err != nil {
		t.Fatalf("AppendMessagesDual(nil): %v", err)
	}
	if mr.Exists("msgs:s-1") || mr.Exists("msgs:buffer") {
		t.Fatal("empty dual write must not create any key")
	}
}

// TestPushMessageBuffer_NoTTL verifies the bare queue producer stages records
// without a TTL and without touching the read cache.
func TestPushMessageBuffer_NoTTL(t *testing.T) {
	store, mr := newTestStore(t, time.Hour)
	ctx := context.Background()

	if err := store.PushMessageBuffer(ctx, [][]byte{[]byte("a"), []byte("b")}); err != nil {
		t.Fatalf("PushMessageBuffer: %v", err)
	}
	if n, _ := store.LenMessageBuffer(ctx); n != 2 {
		t.Fatalf("buffer len = %d, want 2", n)
	}
	if ttl := mr.TTL("msgs:buffer"); ttl != 0 {
		t.Fatalf("buffer key must NOT carry a TTL, got %v", ttl)
	}
	if mr.Exists("msgs:s-1") {
		t.Fatal("PushMessageBuffer must not touch the read cache")
	}
}

// TestMoveMessageToProcessing_ClaimOrderAndDrain verifies the LMOVE claim:
// records come off the buffer head in FIFO order, land in processing, and a
// drained queue reports nil.
func TestMoveMessageToProcessing_ClaimOrderAndDrain(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	raws := [][]byte{[]byte("m1"), []byte("m2"), []byte("m3")}
	if err := store.PushMessageBuffer(ctx, raws); err != nil {
		t.Fatalf("PushMessageBuffer: %v", err)
	}

	for i, want := range raws {
		got, err := store.MoveMessageToProcessing(ctx)
		if err != nil {
			t.Fatalf("MoveMessageToProcessing[%d]: %v", i, err)
		}
		if got == nil {
			t.Fatalf("claim[%d] = nil, want %q", i, want)
		}
		if string(got) != string(want) {
			t.Errorf("claim[%d] = %q, want %q", i, got, want)
		}
	}

	if n, _ := store.LenMessageBuffer(ctx); n != 0 {
		t.Fatalf("buffer len after claims = %d, want 0", n)
	}
	if n := store.Client().LLen(ctx, "msgs:processing").Val(); n != 3 {
		t.Fatalf("processing len = %d, want 3", n)
	}

	// Drained queue: nil, nil — not an error.
	got, err := store.MoveMessageToProcessing(ctx)
	if err != nil {
		t.Fatalf("claim on drained queue returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("claim on drained queue = %q, want nil", got)
	}
}

// TestRemoveFromProcessing_Ack verifies LREM-by-value acknowledgement removes
// exactly the claimed record and leaves the others parked.
func TestRemoveFromProcessing_Ack(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	raws := [][]byte{[]byte("m1"), []byte("m2"), []byte("m3")}
	if err := store.PushMessageBuffer(ctx, raws); err != nil {
		t.Fatal(err)
	}
	var claimed [][]byte
	for range raws {
		raw, err := store.MoveMessageToProcessing(ctx)
		if err != nil {
			t.Fatal(err)
		}
		claimed = append(claimed, raw)
	}

	// Ack the middle record only.
	if err := store.RemoveFromProcessing(ctx, claimed[1]); err != nil {
		t.Fatalf("RemoveFromProcessing: %v", err)
	}
	got, err := store.Client().LRange(ctx, "msgs:processing", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "m1" || got[1] != "m3" {
		t.Fatalf("processing after ack = %v, want [m1 m3]", got)
	}
}

// TestRecoverProcessingToBuffer_PreservesOrder verifies the crash-recovery
// requeue: processing residue re-enters the buffer HEAD in the original claim
// order, ahead of any records that arrived meanwhile, and processing ends up
// empty.
func TestRecoverProcessingToBuffer_PreservesOrder(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	// Claim three records (they park in processing in claim order), then let
	// fresh records accumulate in the buffer behind them.
	raws := [][]byte{[]byte("p1"), []byte("p2"), []byte("p3")}
	if err := store.PushMessageBuffer(ctx, raws); err != nil {
		t.Fatal(err)
	}
	for range raws {
		if _, err := store.MoveMessageToProcessing(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PushMessageBuffer(ctx, [][]byte{[]byte("fresh")}); err != nil {
		t.Fatal(err)
	}

	if err := store.RecoverProcessingToBuffer(ctx); err != nil {
		t.Fatalf("RecoverProcessingToBuffer: %v", err)
	}

	if n, _ := store.LenMessageBuffer(ctx); n != 4 {
		t.Fatalf("buffer len after recovery = %d, want 4", n)
	}
	got, err := store.Client().LRange(ctx, "msgs:buffer", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p1", "p2", "p3", "fresh"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buffer[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// Subsequent claims replay p1..p3 first.
	for i, w := range []string{"p1", "p2", "p3", "fresh"} {
		raw, err := store.MoveMessageToProcessing(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != w {
			t.Fatalf("post-recovery claim[%d] = %q, want %q", i, raw, w)
		}
	}
}

// TestRecoverProcessingToBuffer_EmptyIsNoop verifies recovery on an empty
// processing list returns cleanly.
func TestRecoverProcessingToBuffer_EmptyIsNoop(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)

	if err := store.RecoverProcessingToBuffer(context.Background()); err != nil {
		t.Fatalf("RecoverProcessingToBuffer on empty: %v", err)
	}
}
