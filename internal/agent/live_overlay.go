package agent

import "strings"

const (
	liveReplayContentRunes = 2000
	liveReplayMarker       = "\n... [live message text truncated; full text is preserved in the compacted checkpoint] ...\n"
)

type livePlacement struct {
	message     Message
	sourceIndex int
}

// ApplyLiveOverlay restores current-turn messages whose live-only fields
// (principally inline image bytes) cannot be reconstructed from persistence.
// source is the durable/recovered message sequence and projected is the
// model-facing candidate built from it (anchor + checkpoint + retained tail).
//
// Live messages are matched to the LATEST unfilled source slot that has the
// same persisted identity. A retained message is replaced in place with the
// full live value; a message that moved into the checkpoint is replayed as a
// bounded excerpt. The suffix walk keeps repeated prompts/uploads distinct:
// only the retained occurrence can replace a slot, while summarized text is
// inserted without mutating an older same-content message.
func ApplyLiveOverlay(source, projected, live []Message) []Message {
	if len(live) == 0 {
		return append([]Message(nil), projected...)
	}

	placements := placeLiveMessages(source, live)
	sourceWithLive := append([]Message(nil), source...)
	for _, placement := range placements {
		if placement.sourceIndex >= 0 {
			sourceWithLive[placement.sourceIndex] = placement.message
		}
	}

	suffix := persistedSuffix(sourceWithLive, projected)
	tailSourceStart := len(sourceWithLive) - suffix
	tailProjectedStart := len(projected) - suffix

	bySource := make(map[int]Message, len(placements))
	for _, placement := range placements {
		if placement.sourceIndex >= 0 {
			bySource[placement.sourceIndex] = placement.message
		}
	}

	out := append([]Message(nil), projected[:tailProjectedStart]...)
	anchorSource := -1
	if tailProjectedStart > 0 && len(sourceWithLive) > 0 &&
		samePersistedIdentity(sourceWithLive[0], out[0]) {
		anchorSource = 0
		if replacement, ok := bySource[0]; ok {
			out[0] = replacement
		}
	}

	// All unmatched placements that precede the retained suffix are
	// summarized live messages. Keep them in source order immediately before
	// the tail, after the anchor/checkpoint frame.
	for _, placement := range placements {
		if placement.sourceIndex < 0 || placement.sourceIndex >= tailSourceStart ||
			placement.sourceIndex == anchorSource {
			continue
		}
		out = append(out, replayableLiveMessage(placement.message))
	}

	tail := append([]Message(nil), projected[tailProjectedStart:]...)
	for offset := range tail {
		if replacement, ok := bySource[tailSourceStart+offset]; ok {
			tail[offset] = replacement
		}
	}
	out = append(out, tail...)

	// A live message with no durable counterpart cannot replace an older
	// same-content row. Preserve it as an append-only bounded replay instead.
	for _, placement := range placements {
		if placement.sourceIndex < 0 {
			out = append(out, replayableLiveMessage(placement.message))
		}
	}
	return out
}

func placeLiveMessages(source, live []Message) []livePlacement {
	placements := make([]livePlacement, len(live))
	used := make(map[int]struct{}, len(live))
	// Assign from the end so repeated live messages keep their relative order
	// while each takes the latest still-unfilled matching source slot.
	for i := len(live) - 1; i >= 0; i-- {
		placements[i] = livePlacement{message: live[i], sourceIndex: -1}
		for sourceIndex := len(source) - 1; sourceIndex >= 0; sourceIndex-- {
			if _, taken := used[sourceIndex]; taken {
				continue
			}
			if liveMatchesStoredMessage(source[sourceIndex], live[i]) {
				placements[i].sourceIndex = sourceIndex
				used[sourceIndex] = struct{}{}
				break
			}
		}
	}
	return placements
}

func persistedSuffix(source, projected []Message) int {
	suffix := 0
	for suffix < len(source) && suffix < len(projected) {
		stored := source[len(source)-1-suffix]
		candidate := projected[len(projected)-1-suffix]
		if !samePersistedIdentity(stored, candidate) {
			break
		}
		suffix++
	}
	return suffix
}

func liveMatchesStoredMessage(stored, live Message) bool {
	return stored.Role == "user" && live.Role == "user" &&
		userContentMatches(stored.Content, live)
}

// samePersistedIdentity compares the fields persistence can reconstruct.
// Retained tool result bodies may have been replaced by their deterministic
// summaries, so their stable call identity is authoritative.
func samePersistedIdentity(stored, candidate Message) bool {
	if stored.Role != candidate.Role || stored.ToolCallID != candidate.ToolCallID {
		return false
	}
	switch stored.Role {
	case "user":
		return userContentMatches(candidate.Content, stored)
	case "tool":
		return stored.Name == candidate.Name
	default:
		return stored.Content == candidate.Content &&
			stored.ReasoningContent == candidate.ReasoningContent &&
			equalToolCalls(stored.ToolCalls, candidate.ToolCalls)
	}
}

// userContentMatches accepts the live text plus the deterministic attachment
// placeholder lines persistence appends. Extra non-placeholder text is never
// treated as the same message, preventing same-prefix prompts from aliasing.
func userContentMatches(candidateContent string, live Message) bool {
	if strings.HasPrefix(candidateContent, CompactedSummaryLeadIn) &&
		strings.Contains(candidateContent, "<compacted-summary>") {
		return false
	}
	if candidateContent == live.Content {
		return true
	}
	if len(live.Images) == 0 || !strings.HasPrefix(candidateContent, live.Content) {
		return false
	}
	filenames := imageFilenames(live.Images)
	if len(filenames) == 0 || !containsAllFilenames(candidateContent, filenames) {
		return false
	}
	remainder := strings.TrimPrefix(candidateContent, live.Content)
	if !strings.HasPrefix(remainder, "\n") {
		return false
	}
	for _, line := range strings.Split(strings.TrimPrefix(remainder, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[图片: ") || !strings.HasSuffix(line, "]") {
			return false
		}
	}
	return true
}

func imageFilenames(images []MessageImage) []string {
	out := make([]string, 0, len(images))
	for _, img := range images {
		if img.Filename != "" {
			out = append(out, img.Filename)
		}
	}
	return out
}

func containsAllFilenames(content string, filenames []string) bool {
	for _, filename := range filenames {
		if !strings.Contains(content, filename) {
			return false
		}
	}
	return true
}

func equalToolCalls(a, b []ToolCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID ||
			a[i].Function.Name != b[i].Function.Name ||
			a[i].Function.Arguments != b[i].Function.Arguments {
			return false
		}
	}
	return true
}

func collectLiveMessages(original []Message) []Message {
	live := make([]Message, 0)
	for i, msg := range original {
		isFinalUser := i == len(original)-1 && msg.Role == "user"
		if len(msg.Images) == 0 && !isFinalUser {
			continue
		}
		live = append(live, msg)
	}
	return live
}

func restoreLiveMessages(original, compacted []Message) []Message {
	return ApplyLiveOverlay(original, compacted, collectLiveMessages(original))
}

func replayableLiveMessage(live Message) Message {
	runes := []rune(live.Content)
	if len(runes) <= liveReplayContentRunes {
		return live
	}
	head := liveReplayContentRunes * 3 / 4
	tail := liveReplayContentRunes - head
	live.Content = string(runes[:head]) + liveReplayMarker + string(runes[len(runes)-tail:])
	return live
}
