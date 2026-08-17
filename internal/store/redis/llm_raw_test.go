package redis

import (
	"context"
	"testing"
	"time"
)

func TestRawBuffer_PushPopFIFO(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	recs := [][]byte{
		[]byte(`{"seq":1}`),
		[]byte(`{"seq":2}`),
		[]byte(`{"seq":3}`),
	}
	for _, r := range recs {
		if err := store.PushRawLog(ctx, r); err != nil {
			t.Fatalf("PushRawLog: %v", err)
		}
	}

	n, err := store.LenRawLogs(ctx)
	if err != nil {
		t.Fatalf("LenRawLogs: %v", err)
	}
	if n != 3 {
		t.Fatalf("LenRawLogs = %d, want 3", n)
	}

	// Pop a batch smaller than the queue: head (oldest) first, remainder stays.
	got, err := store.PopRawLogs(ctx, 2)
	if err != nil {
		t.Fatalf("PopRawLogs: %v", err)
	}
	if len(got) != 2 || string(got[0]) != `{"seq":1}` || string(got[1]) != `{"seq":2}` {
		t.Fatalf("PopRawLogs batch = %v, want seq 1 then 2", got)
	}

	n, err = store.LenRawLogs(ctx)
	if err != nil {
		t.Fatalf("LenRawLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("LenRawLogs after pop = %d, want 1", n)
	}

	// Popping more than remains drains the queue without error.
	got, err = store.PopRawLogs(ctx, 50)
	if err != nil {
		t.Fatalf("PopRawLogs drain: %v", err)
	}
	if len(got) != 1 || string(got[0]) != `{"seq":3}` {
		t.Fatalf("PopRawLogs drain = %v, want seq 3", got)
	}

	// Empty buffer pops return an empty slice, not an error.
	got, err = store.PopRawLogs(ctx, 10)
	if err != nil {
		t.Fatalf("PopRawLogs empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("PopRawLogs empty = %v, want empty", got)
	}
}

func TestRawBuffer_NoTTL(t *testing.T) {
	// The buffer is a queue, not a cache: the store TTL must never apply to
	// it, or not-yet-flushed records would silently expire.
	store, mr := newTestStore(t, time.Hour)
	ctx := context.Background()

	if err := store.PushRawLog(ctx, []byte(`{}`)); err != nil {
		t.Fatalf("PushRawLog: %v", err)
	}
	mr.FastForward(2 * time.Hour)

	n, err := store.LenRawLogs(ctx)
	if err != nil {
		t.Fatalf("LenRawLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("LenRawLogs after FastForward = %d, want 1 (buffer must not expire)", n)
	}
}
