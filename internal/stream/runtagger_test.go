package stream

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drainOne(t *testing.T, h *Hub) StreamEvent {
	t.Helper()
	select {
	case e := <-h.Events():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived on the inner hub")
		return StreamEvent{}
	}
}

// TestTaggedWithRunID_TagsEveryEvent: every event sent through the tagged view
// arrives on the INNER hub's consumer channel carrying
// Meta[MetaParentToolCallID]=runID — including events constructed with a nil
// Meta (agent_start/agent_end/token builders emit those), which must be
// initialized rather than skipped (task 2.1 / spec: all sub-agent events carry
// the invocation id).
func TestTaggedWithRunID_TagsEveryEvent(t *testing.T) {
	inner := NewHub(0)
	defer inner.Close()
	tagged := TaggedWithRunID(inner, "call_x1")

	assert.True(t, tagged.Send(AgentStartEvent("Chongzhi")))
	e := drainOne(t, inner)
	assert.Equal(t, EventAgentStart, e.Type)
	assert.Equal(t, "call_x1", e.Meta[MetaParentToolCallID], "nil-meta events must be initialized and tagged")

	assert.True(t, tagged.Send(TokenEvent("Chongzhi", "hi")))
	e = drainOne(t, inner)
	assert.Equal(t, "call_x1", e.Meta[MetaParentToolCallID])

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.True(t, tagged.SendCtx(ctx, ToolCallEvent("Chongzhi", "tc_9", "xizhi_read_file", nil)))
	e = drainOne(t, inner)
	assert.Equal(t, "call_x1", e.Meta[MetaParentToolCallID])
}

// TestTaggedWithRunID_DoesNotOverwriteExistingStamp: an event that already
// carries the key passes through untouched — first stamp wins, so a dispatcher
// sharing the raw hub with a tagged view can never be re-stamped.
func TestTaggedWithRunID_DoesNotOverwriteExistingStamp(t *testing.T) {
	inner := NewHub(0)
	defer inner.Close()
	tagged := TaggedWithRunID(inner, "call_x1")

	pre := AgentErrorEvent("Chongzhi", "boom", "llm_error")
	pre.Meta[MetaParentToolCallID] = "call_original"
	assert.True(t, tagged.Send(pre))

	e := drainOne(t, inner)
	assert.Equal(t, "call_original", e.Meta[MetaParentToolCallID], "existing value must not be overwritten")
}

// TestTaggedWithRunID_MetaMapNotMutatedInPlace: a populated Meta map shared by
// the caller is copied before stamping, so the caller's map (and any other
// event holding it) is never modified.
func TestTaggedWithRunID_MetaMapNotMutatedInPlace(t *testing.T) {
	inner := NewHub(0)
	defer inner.Close()
	tagged := TaggedWithRunID(inner, "call_x1")

	shared := map[string]any{MetaToolCallID: "tc_1"}
	assert.True(t, tagged.Send(StreamEvent{Type: EventToolResult, Agent: "Chongzhi", Meta: shared}))

	_, stillThere := shared[MetaParentToolCallID]
	assert.False(t, stillThere, "caller-shared map must not gain the stamp in place")
	e := drainOne(t, inner)
	assert.Equal(t, "call_x1", e.Meta[MetaParentToolCallID])
	assert.Equal(t, "tc_1", e.Meta[MetaToolCallID], "pre-existing keys must survive the copy")
}

// TestTaggedWithRunID_EmptyRunIDReturnsInner: an empty run id means "no run
// identity" (top-level runs) — the caller gets the inner hub back unchanged,
// so no allocation and no tagging happen.
func TestTaggedWithRunID_EmptyRunIDReturnsInner(t *testing.T) {
	inner := NewHub(0)
	defer inner.Close()
	assert.Same(t, inner, TaggedWithRunID(inner, ""), "empty run id must return the inner hub")
}

// TestTaggedWithRunID_LifecycleDelegatesToInner: the tagged view holds no
// lifecycle of its own — closing the INNER hub makes the view's sends fail
// exactly like raw sends (delegated fast-fail), proving shutdown semantics
// pass through (task 2.1: lifecycle methods pass through).
func TestTaggedWithRunID_LifecycleDelegatesToInner(t *testing.T) {
	inner := NewHub(0)
	tagged := TaggedWithRunID(inner, "call_x1")
	inner.Close()

	assert.False(t, tagged.Send(TokenEvent("Chongzhi", "x")), "send on closed inner hub must fail")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.False(t, tagged.SendCtx(ctx, TokenEvent("Chongzhi", "x")), "sendCtx on closed inner hub must fail")

	// A cancelled context must also win through the view, exactly as on a raw hub.
	inner2 := NewHub(0)
	defer inner2.Close()
	tagged2 := TaggedWithRunID(inner2, "call_x1")
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	assert.False(t, tagged2.SendCtx(cancelled, TokenEvent("Chongzhi", "x")))
}

// TestTaggedWithRunID_BufferFullDropsLikeInner: the view delegates Send's
// drop-on-full behavior — a full buffer drops (returns true, warns) rather
// than blocking, byte-for-byte the inner Hub's semantics.
func TestTaggedWithRunID_BufferFullDropsLikeInner(t *testing.T) {
	inner := NewHub(1)
	defer inner.Close()
	tagged := TaggedWithRunID(inner, "call_x1")

	require.True(t, tagged.Send(TokenEvent("Chongzhi", "first")))  // fills the buffer
	require.True(t, tagged.Send(TokenEvent("Chongzhi", "second"))) // dropped, not blocked
	e := drainOne(t, inner)
	assert.Equal(t, "first", e.Content)
	select {
	case e := <-inner.Events():
		t.Fatalf("second event should have been dropped, got %q", e.Content)
	default:
	}
}
