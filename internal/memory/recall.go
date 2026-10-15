package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	openviking "github.com/volcengine/OpenViking/sdk/go"

	"github.com/lush/blowball/internal/tokens"
)

// recallLeadIn tells the model what the block is and how to treat it: rely
// on the facts without re-asking, but never recite the frame itself.
const recallLeadIn = "Long-term memories about this user, recalled from earlier sessions by relevance to the new message. Treat them as background context; rely on them without re-asking, but do not recite them."

// maxRecallEntryRunes caps a single rendered entry. Abstract (L0) is a
// one-sentence summary by construction, but an Overview fallback can run a
// few thousand tokens — the per-entry cap keeps one chatty memory from
// eating the whole budget before any other entry lands.
const maxRecallEntryRunes = 600

// RecallEntry is one recall hit that survived rendering and dedupe
// filtering, carrying the machine-readable key the handler tracks for
// session-scoped incremental injection.
type RecallEntry struct {
	// Key is the hit's stable identity: the memory URI when non-empty,
	// otherwise a hash of the normalized entry text (URI-less hits are
	// rare; the hash still dedupes content-identical hits).
	Key string
	// Line is the score-prefixed rendered entry exactly as it appears in
	// the frame, so the handler never re-renders (byte-stable replay).
	Line string
}

// renderRecallEntries builds the incremental set of recall entries from
// retrieval hits. Hits whose key is in injected (already injected earlier in
// this session) are dropped before budget accounting.
//
// Entries render score-first ("- [memory 87%] …", the official OpenViking
// Claude Code plugin's shape) sorted by score descending, Abstract preferred
// over Overview (L0 one-liners pack the most signal per token — the plugin's
// recallPreferAbstract default), each rune-capped. The aggregate block is
// bounded by the shared CJK-aware token estimate: entries that do not fit
// are dropped whole — never URI-hinted, since blowball has no read-back tool
// in v1 and a pointer would be dead weight — with the top-scored NEW entry
// always included, so a tight budget degrades to the single most relevant
// new memory instead of nothing.
func renderRecallEntries(memories []openviking.MatchedContext, budget int, injected map[string]struct{}) []RecallEntry {
	sorted := make([]openviking.MatchedContext, len(memories))
	copy(sorted, memories)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })

	// The frame (tags + lead-in) is part of the budget; the +8 fudge covers
	// the tag lines and newlines around it.
	used := tokens.Estimate(recallLeadIn) + 8
	var entries []RecallEntry
	for _, m := range sorted {
		text := strings.TrimSpace(m.Abstract)
		if text == "" {
			text = strings.TrimSpace(m.Overview)
		}
		if text == "" {
			continue
		}
		key := recallEntryKey(m.URI, text)
		if _, ok := injected[key]; ok {
			continue // already injected earlier in this session: never repeat
		}
		text = strings.ReplaceAll(text, "\n", " ") // keep one entry on one line
		if r := []rune(text); len(r) > maxRecallEntryRunes {
			text = string(r[:maxRecallEntryRunes]) + "…"
		}
		line := fmt.Sprintf("- [memory %d%%] %s", scorePercent(m.Score), text)
		cost := tokens.Estimate(line) + 1 // trailing newline
		if len(entries) > 0 && used+cost > budget {
			continue // budget exhausted: drop whole entries; a cheaper one may still fit
		}
		entries = append(entries, RecallEntry{Key: key, Line: line})
		used += cost
	}
	return entries
}

// renderRecallFrame wraps entries in the framed <user-memories> block; ""
// when nothing survived (the caller then skips injection entirely rather
// than injecting an empty frame).
func renderRecallFrame(entries []RecallEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<user-memories>\n")
	b.WriteString(recallLeadIn)
	for _, e := range entries {
		b.WriteString("\n")
		b.WriteString(e.Line)
	}
	b.WriteString("\n</user-memories>")
	return b.String()
}

// recallEntryKey derives the dedupe key for one hit: the memory URI when the
// server provides one, otherwise a SHA-256 of the normalized entry text.
func recallEntryKey(uri, text string) string {
	if uri != "" {
		return uri
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// scorePercent renders a retrieval score as a stable 0-100 integer, clamped
// against gateway quirks (a score outside [0,1] should not print as "137%"
// or "-3%").
func scorePercent(score float64) int {
	p := int(score*100 + 0.5)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}
