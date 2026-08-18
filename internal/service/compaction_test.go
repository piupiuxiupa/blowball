package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
)

// compactionFixture builds a synthetic persisted conversation of n agent
// messages where agent message i maps one-to-one to persistence row i (the
// simplest legal attribution). Row IDs and times ascend so composite-cursor
// comparisons behave like production.
func compactionFixture(n int) ([]model.Message, []agent.Message, []int) {
	base := time.Unix(1_700_000_000, 0).UTC()
	rows := make([]model.Message, n)
	msgs := make([]agent.Message, n)
	lastRow := make([]int, n)
	for i := 0; i < n; i++ {
		rows[i] = model.Message{
			ID:        int64(i + 1),
			MsgTime:   base.Add(time.Duration(i) * time.Minute),
			MsgIndex:  i,
			SessionID: "sess-comp",
			TraceID:   "trace-comp",
		}
		msgs[i] = agent.Message{Role: "user", Content: "m" + string(rune('a'+i))}
		if i > 0 {
			msgs[i] = agent.Message{Role: "assistant", Content: "m" + string(rune('a'+i))}
			rows[i].Role = model.RoleAssistant
		} else {
			rows[i].Role = model.RoleUser
		}
		lastRow[i] = i
	}
	return rows, msgs, lastRow
}

func newCompactionTestService(m *fakeMySQLStore, r *fakeRedisStore, llm agent.LLMClient, maxTokens int) *CompactionService {
	return NewCompactionService(newDeps(m, r, &fakeFSStore{}), llm, config.AgentConfig{
		Name:      "Confucius",
		Model:     "gpt-test",
		MaxTokens: 512,
	}, maxTokens)
}

func compactionInput(rows []model.Message, msgs []agent.Message, lastRow []int) CompactionInput {
	return CompactionInput{
		SessionID:     "sess-comp",
		UserID:        "user-1",
		TriggerKind:   model.CompactionTriggerMidTurn,
		TriggerTokens: 9000,
		Rows:          rows,
		AgentMsgs:     msgs,
		LastRow:       lastRow,
	}
}

func TestCompactionService_ShouldCompact(t *testing.T) {
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	llm := &fakeLLMClient{}
	svc := newCompactionTestService(m, r, llm, 1000)

	if svc.Enabled() == false {
		t.Fatal("configured service must be enabled")
	}
	if got := svc.ThresholdTokens(); got != 800 {
		t.Errorf("ThresholdTokens = %d, want 800 (80%% of 1000)", got)
	}
	cases := []struct {
		tokens int
		want   bool
	}{
		{0, false},   // nothing measured
		{799, false}, // below 80%
		{800, true},  // exact threshold triggers
		{1000, true}, // at the window
	}
	for _, tc := range cases {
		if got := svc.ShouldCompact(tc.tokens); got != tc.want {
			t.Errorf("ShouldCompact(%d) = %v, want %v", tc.tokens, got, tc.want)
		}
	}

	disabled := newCompactionTestService(m, r, llm, 0)
	if disabled.Enabled() {
		t.Fatal("max_context_tokens 0 must disable the service")
	}
	if disabled.ShouldCompact(1_000_000) {
		t.Fatal("disabled service must never trigger")
	}
}

func TestCompactionService_Compact_PreservesFirstAndRetainsTail(t *testing.T) {
	m := &fakeMySQLStore{}
	r := &fakeRedisStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "## Primary Request and Intent\ncheckpoint", Usage: agent.Usage{PromptTokens: 120, CompletionTokens: 30}}}
	svc := newCompactionTestService(m, r, llm, 1000)

	rows, msgs, lastRow := compactionFixture(12) // first + 10 middle + 5 tail? see below
	rec, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow))
	if !ok {
		t.Fatal("Compact returned ok=false; want a record")
	}

	// 12 agent messages: middle = msgs[1:7] (12-5=7), tail = msgs[7:].
	// Boundary = the last shadowed row = the row of msgs[6] = rows[6] (ID 7).
	if rec.BoundaryMsgID != rows[6].ID || rec.BoundaryMsgIndex != rows[6].MsgIndex || !rec.BoundaryMsgTime.Equal(rows[6].MsgTime) {
		t.Errorf("boundary = (%d,%d,%s), want row 7 (%d,%d,%s)",
			rec.BoundaryMsgID, rec.BoundaryMsgIndex, rec.BoundaryMsgTime, rows[6].ID, rows[6].MsgIndex, rows[6].MsgTime)
	}
	if rec.TriggerKind != model.CompactionTriggerMidTurn || rec.TriggerTokens != 9000 {
		t.Errorf("trigger fields = (%q,%d), want (mid_turn,9000)", rec.TriggerKind, rec.TriggerTokens)
	}
	if rec.Content != "## Primary Request and Intent\ncheckpoint" {
		t.Errorf("record content = %q, want the LLM text", rec.Content)
	}
	if rec.SummaryModel != "gpt-test" || rec.SummaryPromptTokens != 120 || rec.SummaryCompletionTokens != 30 {
		t.Errorf("summary observation = (%q,%d,%d)", rec.SummaryModel, rec.SummaryPromptTokens, rec.SummaryCompletionTokens)
	}
	if rec.ShadowedTokens != 120 {
		t.Errorf("ShadowedTokens = %d, want summary prompt size 120", rec.ShadowedTokens)
	}

	// Write order: record insert → redis set → flag update, all fired.
	if len(m.compactions) != 1 {
		t.Fatalf("compactions recorded = %d, want 1", len(m.compactions))
	}
	if r.setCompactionCalls != 1 {
		t.Errorf("redis set calls = %d, want 1", r.setCompactionCalls)
	}
	if m.updateSessionCompactedCalls != 1 {
		t.Errorf("flag update calls = %d, want 1", m.updateSessionCompactedCalls)
	}
}

func TestCompactionService_Compact_TailRetreatsAcrossToolPair(t *testing.T) {
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
	svc := newCompactionTestService(m, &fakeRedisStore{}, llm, 1000)

	// Layout: [user0, a1, u2, a3(tool_calls), tool4, u5, a6, u7, a8, u9]
	// len=10, nominal tailStart = 5 (last 5 = u5..u9); msgs[5] is a user
	// message so no retreat would fire — instead build the split-inside-pair
	// shape: make the 5th-from-last a tool result whose assistant call is
	// 6th-from-last.
	rows, msgs, lastRow := compactionFixture(10)
	msgs[4] = agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Function: agent.ToolCallFunction{Name: "t", Arguments: "{}"}}}}
	msgs[5] = agent.Message{Role: "tool", Content: "out", ToolCallID: "c1", Name: "t"}
	// msgs[5] (6th from last of 10) is the tool result; tailStart retreats
	// from 5 to 4 so the pair stays together in the tail.
	_, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow))
	if !ok {
		t.Fatal("Compact returned ok=false")
	}
	rec := m.compactions[0]
	// middle = msgs[1:4]; boundary = row of msgs[3] = rows[3] (ID 4).
	if rec.BoundaryMsgID != rows[3].ID {
		t.Errorf("boundary msg id = %d, want %d (pair retreated out of the middle)", rec.BoundaryMsgID, rows[3].ID)
	}
}

func TestCompactionService_Compact_EmptyMiddleSkips(t *testing.T) {
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
	svc := newCompactionTestService(m, &fakeRedisStore{}, llm, 1000)

	// Exactly first + 5 tail messages: nothing compressible.
	rows, msgs, lastRow := compactionFixture(6)
	if _, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow)); ok {
		t.Fatal("Compact compacted a first+tail-only conversation")
	}
	if len(m.compactions) != 0 || llm.gotCall {
		t.Error("no record, redis write, or LLM call may happen on an empty middle")
	}
}

func TestCompactionService_Compact_RecompactionMergesPrior(t *testing.T) {
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "merged checkpoint"}}
	svc := newCompactionTestService(m, &fakeRedisStore{}, llm, 1000)

	rows, msgs, lastRow := compactionFixture(20)
	// Prior compaction shadowed through row index 7 (ID 8).
	m.compactions = append(m.compactions, model.ContextCompaction{
		ID:               1,
		SessionID:        "sess-comp",
		Content:          "PRIOR SUMMARY",
		BoundaryMsgTime:  rows[7].MsgTime,
		BoundaryMsgIndex: rows[7].MsgIndex,
		BoundaryMsgID:    rows[7].ID,
	})

	rec, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow))
	if !ok {
		t.Fatal("Compact returned ok=false on re-compaction")
	}
	if len(m.compactions) != 2 {
		t.Fatalf("records = %d, want 2 (append-only)", len(m.compactions))
	}
	// New scope starts after the prior boundary: middle = msgs[8:15]
	// (tailStart = 20-5 = 15). Boundary = row of msgs[14] = rows[14] (ID 15).
	if rec.BoundaryMsgID != rows[14].ID {
		t.Errorf("boundary msg id = %d, want %d", rec.BoundaryMsgID, rows[14].ID)
	}
	// The summary input must merge the prior checkpoint, not stack it.
	prompt := llm.lastReq.Messages[1].Content
	if !strings.Contains(prompt, "PRIOR CHECKPOINT") || !strings.Contains(prompt, "PRIOR SUMMARY") {
		t.Error("summary input missing the prior checkpoint merge block")
	}
	if strings.Contains(prompt, "m"+string(rune('a'+1))) {
		// msgs[1] content "mb" — already condensed by the prior record; it
		// must NOT be re-fed into the summary.
		t.Error("summary input re-feeds previously shadowed content")
	}
	// Latest record wins for stitching.
	if latest, _ := m.LatestCompaction(context.Background(), "sess-comp"); latest.ID != rec.ID {
		t.Errorf("LatestCompaction id = %d, want the new record %d", latest.ID, rec.ID)
	}
}

func TestCompactionService_Compact_SummaryFailureDegrades(t *testing.T) {
	for name, llm := range map[string]*fakeLLMClient{
		"llm error":  {err: errors.New("gateway 500")},
		"empty text": {resp: agent.LLMResponse{Content: "   "}},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeMySQLStore{}
			r := &fakeRedisStore{}
			svc := newCompactionTestService(m, r, llm, 1000)
			rows, msgs, lastRow := compactionFixture(12)
			if _, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow)); ok {
				t.Fatal("Compact reported success on summary failure")
			}
			if len(m.compactions) != 0 || r.setCompactionCalls != 0 || m.updateSessionCompactedCalls != 0 {
				t.Error("no record/cache/flag write may happen on summary failure")
			}
		})
	}
}

func TestCompactionService_Compact_InsertFailureAborts(t *testing.T) {
	m := &fakeMySQLStore{insertCompactionErr: errFake}
	r := &fakeRedisStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
	svc := newCompactionTestService(m, r, llm, 1000)

	rows, msgs, lastRow := compactionFixture(12)
	if _, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow)); ok {
		t.Fatal("Compact reported success despite record insert failure")
	}
	if r.setCompactionCalls != 0 || m.updateSessionCompactedCalls != 0 {
		t.Error("insert failure must abort before the cache write and flag update")
	}
}

func TestCompactionService_Compact_CacheAndFlagFailureStillDegrade(t *testing.T) {
	rows, msgs, lastRow := compactionFixture(12)

	t.Run("redis write fails", func(t *testing.T) {
		m := &fakeMySQLStore{}
		r := &fakeRedisStore{setCompactionErr: errFake}
		llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
		svc := newCompactionTestService(m, r, llm, 1000)
		rec, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow))
		if !ok {
			t.Fatal("record is durable; compaction must still succeed")
		}
		if len(m.compactions) != 1 || rec == nil {
			t.Error("record must be inserted and returned")
		}
		if m.updateSessionCompactedCalls != 1 {
			t.Error("flag update must still run after a cache-write failure")
		}
	})

	t.Run("flag update fails", func(t *testing.T) {
		m := &fakeMySQLStore{updateSessionCompactedErr: errFake}
		r := &fakeRedisStore{}
		llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
		svc := newCompactionTestService(m, r, llm, 1000)
		if _, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow)); !ok {
			t.Fatal("record + cache are durable; flag failure must not fail the compaction")
		}
	})
}

func TestCompactionService_Compact_DisabledNeverCompacts(t *testing.T) {
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
	svc := newCompactionTestService(m, &fakeRedisStore{}, llm, 0)
	rows, msgs, lastRow := compactionFixture(50)
	if _, ok := svc.Compact(context.Background(), compactionInput(rows, msgs, lastRow)); ok {
		t.Fatal("disabled service compacted")
	}
}

func TestCompactionService_Compact_MalformedAttributionSkips(t *testing.T) {
	m := &fakeMySQLStore{}
	llm := &fakeLLMClient{resp: agent.LLMResponse{Content: "checkpoint"}}
	svc := newCompactionTestService(m, &fakeRedisStore{}, llm, 1000)
	rows, msgs, lastRow := compactionFixture(12)
	in := compactionInput(rows, msgs, lastRow)
	in.LastRow = in.LastRow[:5] // out of sync with AgentMsgs
	if _, ok := svc.Compact(context.Background(), in); ok {
		t.Fatal("Compact accepted mismatched attribution")
	}
}

func TestCompactionService_LatestCompactionRecord(t *testing.T) {
	rec := model.ContextCompaction{ID: 7, SessionID: "s", Content: "c"}
	raw, _ := json.Marshal(rec)

	t.Run("redis hit serves without mysql", func(t *testing.T) {
		m := &fakeMySQLStore{latestCompactionErr: errors.New("mysql must not be read on cache hit")}
		r := &fakeRedisStore{compactionCache: raw}
		svc := newCompactionTestService(m, r, &fakeLLMClient{}, 1000)
		got := svc.LatestCompactionRecord(context.Background(), "s")
		if got == nil || got.ID != 7 {
			t.Fatalf("record = %+v, want id 7 from cache", got)
		}
	})

	t.Run("miss reads mysql and backfills", func(t *testing.T) {
		m := &fakeMySQLStore{}
		m.compactions = []model.ContextCompaction{rec}
		r := &fakeRedisStore{}
		svc := newCompactionTestService(m, r, &fakeLLMClient{}, 1000)
		got := svc.LatestCompactionRecord(context.Background(), "s")
		if got == nil || got.ID != 7 {
			t.Fatalf("record = %+v, want mysql row", got)
		}
		if r.setCompactionCalls != 1 {
			t.Error("cache must be backfilled after a miss")
		}
	})

	t.Run("double miss returns nil", func(t *testing.T) {
		svc := newCompactionTestService(&fakeMySQLStore{}, &fakeRedisStore{}, &fakeLLMClient{}, 1000)
		if got := svc.LatestCompactionRecord(context.Background(), "s"); got != nil {
			t.Fatalf("record = %+v, want nil", got)
		}
	})

	t.Run("redis error degrades to mysql", func(t *testing.T) {
		m := &fakeMySQLStore{}
		m.compactions = []model.ContextCompaction{rec}
		r := &fakeRedisStore{getCompactionErr: errFake}
		svc := newCompactionTestService(m, r, &fakeLLMClient{}, 1000)
		if got := svc.LatestCompactionRecord(context.Background(), "s"); got == nil || got.ID != 7 {
			t.Fatalf("record = %+v, want mysql fallback", got)
		}
	})
}

func TestCompactionService_RenderAgentTranscript(t *testing.T) {
	msgs := []agent.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{Function: agent.ToolCallFunction{Name: "t", Arguments: `{"x":1}`}}}},
		{Role: "tool", Content: "out", Name: "t"},
		{Role: "assistant", Content: "done"},
	}
	got := renderAgentTranscript(msgs)
	for _, want := range []string{"USER:\nhello", "ASSISTANT TOOL CALL t(", `{"x":1}`, "TOOL RESULT t:\nout", "ASSISTANT:\ndone"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
}
