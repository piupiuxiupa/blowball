package agent

import (
	"context"
	"testing"

	"github.com/lush/blowball/internal/pkg/trace"
	"github.com/lush/blowball/internal/tool/skill"
)

func TestSessionIDContext(t *testing.T) {
	ctx := context.Background()
	if got := SessionIDFromContext(ctx); got != "" {
		t.Fatalf("SessionIDFromContext(empty) = %q, want empty", got)
	}
	ctx = WithSessionID(ctx, "sess-1")
	if got := SessionIDFromContext(ctx); got != "sess-1" {
		t.Fatalf("SessionIDFromContext = %q, want sess-1", got)
	}
	// Empty value is a no-op (matching trace.WithContext).
	if got := SessionIDFromContext(WithSessionID(ctx, "")); got != "sess-1" {
		t.Fatalf("WithSessionID(empty) overrode previous value: %q", got)
	}
}

func TestAgentNameContext(t *testing.T) {
	ctx := context.Background()
	if got := AgentNameFromContext(ctx); got != "" {
		t.Fatalf("AgentNameFromContext(empty) = %q, want empty", got)
	}
	ctx = WithAgentName(ctx, "Confucius")
	if got := AgentNameFromContext(ctx); got != "Confucius" {
		t.Fatalf("AgentNameFromContext = %q, want Confucius", got)
	}
	// Sub-agents re-wrap with their own name, shadowing the parent's.
	ctx = WithAgentName(ctx, "Chongzhi")
	if got := AgentNameFromContext(ctx); got != "Chongzhi" {
		t.Fatalf("re-wrap = %q, want Chongzhi", got)
	}
}

func TestCaptureMeta(t *testing.T) {
	ctx := context.Background()
	ctx = trace.WithContext(ctx, "trace-1")
	ctx = WithSessionID(ctx, "sess-1")
	ctx = WithAgentName(ctx, "Liang")
	ctx = skill.WithUserID(ctx, "user-1")

	traceID, sessionID, userID, agentName := captureMeta(ctx)
	if traceID != "trace-1" || sessionID != "sess-1" || userID != "user-1" || agentName != "Liang" {
		t.Fatalf("captureMeta = (%q,%q,%q,%q), want (trace-1,sess-1,user-1,Liang)",
			traceID, sessionID, userID, agentName)
	}

	// Missing agent name falls back to "unknown" so rows are attributable.
	_, _, _, agentName = captureMeta(context.Background())
	if agentName != "unknown" {
		t.Fatalf("captureMeta fallback agent = %q, want unknown", agentName)
	}
}
