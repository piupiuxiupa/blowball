package tokens

import (
	"strings"
	"testing"
)

// TestEstimate pins the CJK-aware heuristic: CJK runes at 1 token each,
// everything else at ceil(chars/4), empty input at 0.
func TestEstimate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		// 4 CJK runes → 4 tokens (1:1, conservative vs the gateway's
		// ~0.6-0.75 tokens/char).
		{"pure CJK", "你好世界", 4},
		{"pure Japanese kana", "こんにちは", 5},
		{"pure Korean hangul", "안녕하세요", 5},
		// Exactly 4 chars/4 = 1 token.
		{"pure ASCII multiple of four", "abcd", 1},
		// 8 chars → 2 tokens.
		{"pure ASCII eight", "abcdefgh", 2},
		// Ceiling, not floor: 1 char still rounds up to 1 token (the
		// conservative direction — never estimate below the true count for
		// short fragments).
		{"single ASCII char rounds up", "x", 1},
		{"three ASCII chars round up", "abc", 1},
		// Mixed: 4 CJK (4) + 8 ASCII (2) = 6.
		{"mixed CJK and ASCII", "你好世界abcdefgh", 6},
		// Spaces and punctuation count in the non-CJK bucket.
		{"ASCII with spaces", "hello world!", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Estimate(tc.in); got != tc.want {
				t.Errorf("Estimate(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestEstimate_ConservativeDirection verifies the pure-CJK estimate is never
// below the rune count — the property the abuse guard depends on.
func TestEstimate_ConservativeDirection(t *testing.T) {
	s := strings.Repeat("汉", 5000)
	if got := Estimate(s); got < 5000 {
		t.Errorf("Estimate(5000 CJK runes) = %d, want >= 5000 (never under-count CJK)", got)
	}
}
