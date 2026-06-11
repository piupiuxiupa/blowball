package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestStore starts a fresh miniredis instance and returns a Store pointed
// at it, plus a cleanup function the caller must defer. The TTL is set to a
// generous value so individual tests can opt into TTL behaviour via
// mini.FastForward.
func newTestStore(t *testing.T, ttl time.Duration) (*Store, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)

	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })

	// Skip the live-ping path in New by constructing the Store directly; this
	// keeps tests hermetic and avoids the implicit 5s timeout context.
	return &Store{client: cli, ttl: ttl}, mr
}

func TestSessionCache_SetGetDel(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	want := []byte(`{"session_id":"s-1"}`)
	if err := store.SetSessionCache(ctx, "s-1", want); err != nil {
		t.Fatalf("SetSessionCache: %v", err)
	}

	got, err := store.GetSessionCache(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetSessionCache: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("GetSessionCache = %q, want %q", got, want)
	}

	if err := store.DelSessionCache(ctx, "s-1"); err != nil {
		t.Fatalf("DelSessionCache: %v", err)
	}

	if got, err := store.GetSessionCache(ctx, "s-1"); err != nil {
		t.Fatalf("GetSessionCache after del: %v", err)
	} else if got != nil {
		t.Fatalf("GetSessionCache after del = %q, want nil", got)
	}
}

func TestGetSessionCache_MissReturnsNil(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	got, err := store.GetSessionCache(ctx, "never-seen")
	if err != nil {
		t.Fatalf("GetSessionCache miss returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("GetSessionCache miss = %v, want nil", got)
	}
}

func TestMessageCache_AppendGet(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	msgs := [][]byte{
		[]byte(`{"i":0,"role":"user"}`),
		[]byte(`{"i":1,"role":"assistant"}`),
		[]byte(`{"i":2,"role":"tool"}`),
	}
	for i, m := range msgs {
		if err := store.AppendMessage(ctx, "s-1", m); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("GetMessages len = %d, want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if string(got[i]) != string(msgs[i]) {
			t.Errorf("GetMessages[%d] = %q, want %q", i, got[i], msgs[i])
		}
	}
}

func TestGetMessages_EmptyList(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	got, err := store.GetMessages(ctx, "no-such-session")
	if err != nil {
		t.Fatalf("GetMessages empty returned error: %v", err)
	}
	if got == nil {
		t.Fatal("GetMessages empty returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("GetMessages empty len = %d, want 0", len(got))
	}
}

func TestMessageCache_SetReplaces(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	// Seed the list with stale entries.
	if err := store.AppendMessage(ctx, "s-1", []byte("old-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(ctx, "s-1", []byte("old-2")); err != nil {
		t.Fatal(err)
	}

	want := [][]byte{[]byte("new-1"), []byte("new-2"), []byte("new-3")}
	if err := store.SetMessages(ctx, "s-1", want); err != nil {
		t.Fatalf("SetMessages: %v", err)
	}

	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("GetMessages len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Errorf("GetMessages[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMessageCache_SetEmptyClears(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	if err := store.AppendMessage(ctx, "s-1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	// SetMessages with an empty slice must DELETE the key entirely so callers
	// do not observe stale entries.
	if err := store.SetMessages(ctx, "s-1", nil); err != nil {
		t.Fatalf("SetMessages(nil): %v", err)
	}
	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetMessages len = %d, want 0 after SetMessages(nil)", len(got))
	}
}

func TestMessageCache_ClearRemovesKey(t *testing.T) {
	store, _ := newTestStore(t, time.Hour)
	ctx := context.Background()

	if err := store.AppendMessage(ctx, "s-1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearMessages(ctx, "s-1"); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetMessages len = %d, want 0 after Clear", len(got))
	}
}

// TestSessionCache_TTLExpires uses miniredis.FastForward to verify that the
// TTL is actually applied on SetSessionCache. After advancing the virtual
// clock past the TTL, a subsequent Get must miss.
func TestSessionCache_TTLExpires(t *testing.T) {
	const ttlSecs = 60
	store, mr := newTestStore(t, time.Duration(ttlSecs)*time.Second)
	ctx := context.Background()

	if err := store.SetSessionCache(ctx, "s-1", []byte("payload")); err != nil {
		t.Fatalf("SetSessionCache: %v", err)
	}

	// Sanity check: the key should have a TTL set on it.
	ttl := mr.TTL("session:s-1")
	if ttl <= 0 {
		t.Fatalf("expected positive TTL on session:s-1, got %v", ttl)
	}

	// Advance the virtual clock past the TTL.
	mr.FastForward(ttlSecs * time.Second)

	got, err := store.GetSessionCache(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetSessionCache after TTL: %v", err)
	}
	if got != nil {
		t.Fatalf("GetSessionCache after TTL = %q, want nil (expired)", got)
	}
}

// TestMessageCache_AppendRefreshesTTL verifies the per-append TTL refresh so
// long-running conversations do not silently drop their cache mid-stream.
func TestMessageCache_AppendRefreshesTTL(t *testing.T) {
	const ttlSecs = 60
	store, mr := newTestStore(t, time.Duration(ttlSecs)*time.Second)
	ctx := context.Background()

	if err := store.AppendMessage(ctx, "s-1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	firstTTL := mr.TTL("msgs:s-1")
	if firstTTL <= 0 {
		t.Fatalf("expected positive TTL after first append, got %v", firstTTL)
	}

	// Fast-forward near (but not past) the original expiry.
	mr.FastForward(ttlSecs * 50 / 60 * time.Second)

	// Another append must refresh the TTL.
	if err := store.AppendMessage(ctx, "s-1", []byte("b")); err != nil {
		t.Fatal(err)
	}
	secondTTL := mr.TTL("msgs:s-1")
	if secondTTL <= firstTTL-(30*time.Second) {
		t.Fatalf("expected TTL to refresh after second append; first=%v second=%v", firstTTL, secondTTL)
	}

	// Past the original TTL the key must still be alive thanks to the refresh.
	mr.FastForward(ttlSecs * 50 / 60 * time.Second)
	got, err := store.GetMessages(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages to survive after TTL refresh, got %d", len(got))
	}
}

// TestNew_PingFailsOnBadAddr ensures New surfaces a connection error rather
// than returning a silently-broken Store. We point it at a closed miniredis.
func TestNew_PingFailsOnBadAddr(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	if _, err := New(addr, "", 0, time.Hour); err == nil {
		t.Fatal("New with unreachable redis: expected error, got nil")
	}
}
