package handler

import "github.com/lush/blowball/internal/tokens"

// estimateTokens is the handler-local name for the shared conservative
// CJK-aware token heuristic (see internal/tokens). It is a thin alias so the
// input-limit path (add-input-token-limit) and the mcp_call result-spill
// threshold (mcp-call-result-spill) share one implementation; the behavior is
// unchanged byte-for-byte from the pre-extraction version.
func estimateTokens(s string) int { return tokens.Estimate(s) }
