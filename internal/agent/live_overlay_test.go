package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyLiveOverlay_RepeatedAttachmentUsesRetainedOccurrence(t *testing.T) {
	live := Message{
		Role:    "user",
		Content: "same question",
		Images: []MessageImage{{
			AttachmentID: "att-current",
			Filename:     "current.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	source := []Message{
		{Role: "user", Content: "same question\n[图片: current.png]"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "same question\n[图片: current.png]"},
	}
	projected := append([]Message(nil), source...)

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 3)
	assert.Empty(t, got[0].Images, "an older identical upload must not receive the live bytes")
	require.Len(t, got[2].Images, 1)
	assert.Equal(t, "att-current", got[2].Images[0].AttachmentID)
}

func TestApplyLiveOverlay_SummarizedReplayDoesNotOverwriteOlderMessage(t *testing.T) {
	live := Message{
		Role:    "user",
		Content: "same question",
		Images: []MessageImage{{
			AttachmentID: "att-current",
			Filename:     "current.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	source := []Message{
		{Role: "user", Content: "same question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "same question\n[图片: current.png]"},
	}
	projected := []Message{
		{Role: "user", Content: "same question"},
		{Role: "assistant", Content: "old answer"},
	}

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 3)
	assert.Empty(t, got[0].Images, "a summarized live message must not replace an older same-content row")
	require.Len(t, got[2].Images, 1)
	assert.Equal(t, "att-current", got[2].Images[0].AttachmentID)
}

func TestApplyLiveOverlay_SummarizedReplayStaysBeforeRetainedTail(t *testing.T) {
	live := Message{
		Role:    "user",
		Content: "summarized question",
		Images: []MessageImage{{
			AttachmentID: "att-current",
			Filename:     "current.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	source := []Message{
		{Role: "user", Content: "anchor"},
		{Role: "user", Content: "previous summary"},
		{Role: "user", Content: "summarized question\n[图片: current.png]"},
		{Role: "assistant", Content: "retained answer"},
	}
	projected := []Message{
		{Role: "user", Content: "anchor"},
		{Role: "user", Content: "new summary"},
		{Role: "assistant", Content: "retained answer"},
	}

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 4)
	assert.Equal(t, "new summary", got[1].Content)
	assert.Equal(t, "summarized question", got[2].Content)
	require.Len(t, got[2].Images, 1)
	assert.Equal(t, "retained answer", got[3].Content)
}

func TestApplyLiveOverlay_RetainedLongMessageUsesFullContent(t *testing.T) {
	longText := strings.Repeat("长", 3000)
	live := Message{
		Role:    "user",
		Content: longText,
		Images: []MessageImage{{
			AttachmentID: "att-long",
			Filename:     "long.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	source := []Message{{Role: "user", Content: longText + "\n[图片: long.png]"}}
	projected := append([]Message(nil), source...)

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 1)
	assert.Equal(t, longText, got[0].Content,
		"a retained live message must keep its full text; replay truncation is only for summarized messages")
	require.Len(t, got[0].Images, 1)
}

func TestApplyLiveOverlay_BoundsSummarizedLongMessage(t *testing.T) {
	longText := strings.Repeat("长", 3000)
	live := Message{
		Role:    "user",
		Content: longText,
		Images: []MessageImage{{
			AttachmentID: "att-long",
			Filename:     "long.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	source := []Message{
		{Role: "user", Content: longText + "\n[图片: long.png]"},
		{Role: "assistant", Content: "answer"},
	}
	projected := []Message{{Role: "user", Content: "checkpoint summary"}}

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 2)
	assert.Less(t, len([]rune(got[1].Content)), liveReplayContentRunes+100)
	assert.Contains(t, got[1].Content, "earlier content is summarized in the checkpoint")
	require.Len(t, got[1].Images, 1)
}

func TestApplyLiveOverlay_NoDurableCounterpartPreservesFullContent(t *testing.T) {
	longText := strings.Repeat("problem-", 1000)
	live := Message{Role: "user", Content: longText}
	source := []Message{{Role: "assistant", Content: "unrelated history"}}
	projected := []Message{{Role: "user", Content: "checkpoint summary"}}

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 2)
	assert.Equal(t, longText, got[1].Content,
		"a message absent from both durable history and the checkpoint must never be truncated")
	assert.NotContains(t, got[1].Content, liveReplayMarker)
}

func TestApplyLiveOverlay_UpgradesPriorBoundedReplay(t *testing.T) {
	longText := strings.Repeat("长", 3000)
	live := Message{
		Role:    "user",
		Content: longText,
		Images: []MessageImage{{
			AttachmentID: "att-long",
			Filename:     "long.png",
			MIMEType:     "image/png",
			Data:         []byte{1},
		}},
	}
	replay := replayableLiveMessage(live)
	source := []Message{
		{Role: "user", Content: "anchor"},
		{Role: "user", Content: FrameCompactedSummary("previous")},
		replay,
		{Role: "assistant", Content: "answer"},
	}
	projected := []Message{
		{Role: "user", Content: "anchor"},
		{Role: "user", Content: FrameCompactedSummary("new")},
		{Role: "user", Content: longText + "\n[图片: long.png]"},
		{Role: "assistant", Content: "answer"},
	}

	got := ApplyLiveOverlay(source, projected, []Message{live})

	require.Len(t, got, 4)
	assert.Equal(t, longText, got[2].Content,
		"a bounded replay from the prior compaction must map back to the retained full message")
	require.Len(t, got[2].Images, 1)
}

func TestCallCompactionHook_UsesAppliedMarkerInsteadOfContentMatching(t *testing.T) {
	source := []Message{
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "calling", ToolCalls: []ToolCall{{
			ID:       "call-1",
			Function: ToolCallFunction{Name: "lookup", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Name: "lookup", Content: "result"},
	}
	live := []Message{{Role: "user", Content: "continue"}}
	projected := []Message{{Role: "user", Content: "continue"}}

	got := callCompactionHook(context.Background(), source, live, func(hctx context.Context) []Message {
		MarkLiveOverlayApplied(hctx)
		return projected
	})

	assert.Equal(t, projected, got,
		"the hook-scoped marker must make the durable final projection authoritative; an older same-content row cannot suppress or trigger replay")
}

func TestCallCompactionHook_UnmarkedFallbackKeepsCurrentDuplicate(t *testing.T) {
	source := []Message{
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "previous answer"},
		{Role: "user", Content: "continue"},
		{Role: "assistant", Content: "calling", ToolCalls: []ToolCall{{
			ID:       "call-1",
			Function: ToolCallFunction{Name: "lookup", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Name: "lookup", Content: "result"},
	}
	live := []Message{{Role: "user", Content: "continue"}}
	projected := []Message{{Role: "user", Content: "continue"}}

	got := callCompactionHook(context.Background(), source, live, func(context.Context) []Message {
		return projected
	})

	require.Len(t, got, 2)
	assert.Equal(t, "continue", got[0].Content)
	assert.Equal(t, "continue", got[1].Content,
		"without the marker, source alignment must preserve the current occurrence despite an older same-content message")
}

func TestCollectLiveMessages_UsesCurrentUserAfterToolRound(t *testing.T) {
	original := []Message{
		{Role: "user", Content: "anchor"},
		{Role: "user", Content: FrameCompactedSummary("previous")},
		{Role: "user", Content: "current question"},
		{Role: "assistant", Content: "calling", ToolCalls: []ToolCall{{
			ID:       "call-1",
			Function: ToolCallFunction{Name: "lookup", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Name: "lookup", Content: "result"},
	}

	got := collectLiveMessages(original)

	require.Len(t, got, 1)
	assert.Equal(t, "current question", got[0].Content,
		"the current user question must be identified by position among user turns, not by being the final message")
}
