package redis

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lush/blowball/pkg/run"
)

func TestRunStore_ClaimBackgroundSessionRespectsClaimAndFIFO(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()

	holder, claimed, err := rs.ClaimBackgroundSession(ctx, "sess-1", "scheduled-1")
	if err != nil || !claimed || holder != "scheduled-1" {
		t.Fatalf("first background claim = (%q,%v,%v)", holder, claimed, err)
	}
	holder, claimed, err = rs.ClaimBackgroundSession(ctx, "sess-1", "scheduled-2")
	if err != nil || claimed || holder != "scheduled-1" {
		t.Fatalf("conflicting background claim = (%q,%v,%v)", holder, claimed, err)
	}
	if err := rs.ReleaseSession(ctx, "sess-1", "scheduled-1"); err != nil {
		t.Fatal(err)
	}

	if _, claimed, err := rs.ClaimSession(ctx, "sess-1", "chat-running"); err != nil || !claimed {
		t.Fatalf("seed chat claim: claimed=%v err=%v", claimed, err)
	}
	res, err := rs.AdmitMessage(ctx, "sess-1", redisQueueEntry("chat-queued"), 8)
	if err != nil || res.Outcome != run.AdmitQueued {
		t.Fatalf("seed queued chat = (%+v,%v)", res, err)
	}
	if err := rs.ReleaseSession(ctx, "sess-1", "chat-running"); err != nil {
		t.Fatal(err)
	}

	_, claimed, err = rs.ClaimBackgroundSession(ctx, "sess-1", "scheduled-3")
	if err != nil || claimed {
		t.Fatalf("background claim crossed non-empty FIFO: claimed=%v err=%v", claimed, err)
	}
	depth, err := rs.QueueLen(ctx, "sess-1")
	if err != nil || depth != 1 {
		t.Fatalf("queue depth = (%d,%v), want unchanged 1", depth, err)
	}
}

func TestRunStore_ClaimBackgroundSessionConcurrentSingleWinner(t *testing.T) {
	rs, _ := testRunStore(t)
	ctx := context.Background()
	const contenders = 16
	var winners atomic.Int32
	var wg sync.WaitGroup
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			defer wg.Done()
			_, claimed, err := rs.ClaimBackgroundSession(ctx, "sess-race", "scheduled-race")
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claimed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("background claim winners = %d, want 1", got)
	}
}
