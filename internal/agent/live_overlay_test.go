package agent

import (
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
	assert.Contains(t, got[1].Content, "full text is preserved in the compacted checkpoint")
	require.Len(t, got[1].Images, 1)
}
