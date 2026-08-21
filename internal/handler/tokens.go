package handler

import "unicode"

// nonCJKCharsPerToken is the heuristic density of non-CJK text: ~4
// characters per token for latin-script languages.
const nonCJKCharsPerToken = 4

// estimateTokens returns a conservative heuristic estimate of the token count
// of s (add-input-token-limit). It exists to reject abusive oversized input
// BEFORE any storage or LLM work — not to bill tokens — so it deliberately
// avoids tokenizer dependencies: the gateway's models (glm family) do not use
// a published BPE anyway and the red line itself is a product decision, not a
// metering boundary.
//
// Rule: every CJK-family rune counts as one token (Chinese is ~0.6-0.75
// tokens/char on the gateway, so this over-counts), everything else counts
// ceil(chars/4) (accurate for English). The error direction is intentionally
// high — a borderline input may be rejected one step early, which is the safe
// side for an abuse guard, and the limit is operator-tunable via
// messages.max_input_tokens.
func estimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+nonCJKCharsPerToken-1)/nonCJKCharsPerToken
}

// isCJK reports whether r belongs to the CJK writing families the heuristic
// bills at one token per rune: Han, Kana (Hiragana + Katakana), and Hangul.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
