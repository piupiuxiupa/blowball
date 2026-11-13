package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/tool"
)

func TestRuntimeStatus_CrossTurnReplayAndCacheMiss(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{content: "answer one", finishReason: "stop"},
		scriptedLLMResponse{content: "answer two", finishReason: "stop"},
		scriptedLLMResponse{content: "answer three", finishReason: "stop"},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, "")

	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question one"}`, token).Code)
	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question two"}`, token).Code)
	requests := nonTitleRequests(llm.requests())
	require.Len(t, requests, 2)
	first := requests[0].Messages
	second := requests[1].Messages
	require.Greater(t, len(second), len(first))
	assert.Equal(t, first, second[:len(first)],
		"a Redis hit must preserve the complete previous turn request prefix")

	entries, err := env.redisSvc.ListRuntimeStatus(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	firstStatus := first[1].Content
	secondStatus := second[len(second)-2].Content
	require.Equal(t, agent.RuntimeStatusCurrent, second[len(second)-2].RuntimeStatus)

	require.NoError(t, env.redisSvc.ClearRuntimeStatus(context.Background(), defaultSessionID))
	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question three"}`, token).Code)
	requests = nonTitleRequests(llm.requests())
	require.Len(t, requests, 3)
	statusCount := 0
	for _, message := range requests[2].Messages {
		assert.NotEqual(t, firstStatus, message.Content, "miss must not replay the first old status")
		assert.NotEqual(t, secondStatus, message.Content, "miss must not replay the second old status")
		if message.RuntimeStatus == agent.RuntimeStatusCurrent {
			statusCount++
		}
	}
	assert.Equal(t, 1, statusCount, "a cache miss starts an epoch with one fresh current status")

	rows, err := env.mysqlFake.ListMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	for _, row := range rows {
		assert.NotContains(t, row.Content, "<current_status>", "durable messages must stay status-free")
	}
}

func TestRuntimeStatus_InvalidAnchorStartsFreshEpoch(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{content: "answer one", finishReason: "stop"},
		scriptedLLMResponse{content: "answer two", finishReason: "stop"},
	)
	env := newTestEnv(t, llm)
	token := authToken(t, "")
	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question one"}`, token).Code)
	afterFirst, err := env.redisSvc.ListRuntimeStatus(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.Len(t, afterFirst, 1, "the first turn must append its current status")
	rowsAfterFirst, err := env.mysqlFake.ListMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rowsAfterFirst)
	assert.Equal(t, rowsAfterFirst[0].ClientMsgID, afterFirst[0].BeforeClientMsgID)
	first := nonTitleRequests(llm.requests())[0].Messages
	oldStatus := first[1].Content

	require.NoError(t, env.redisSvc.AppendRuntimeStatus(context.Background(), defaultSessionID, model.RuntimeStatus{
		StatusID: "orphan", Kind: agent.RuntimeStatusCurrent, Content: "orphan", Seq: 99,
		BeforeClientMsgID: "missing-user",
	}))
	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question two"}`, token).Code)
	second := nonTitleRequests(llm.requests())[1].Messages
	for _, message := range second {
		assert.NotEqual(t, oldStatus, message.Content, "an invalid anchor invalidates the whole old log")
	}

	entries, err := env.redisSvc.ListRuntimeStatus(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the fresh epoch contains only the new current status")
	assert.Equal(t, "question two", second[len(second)-1].Content)
}

func TestRuntimeStatus_CompactionSequenceContinuesAcrossTurns(t *testing.T) {
	llm := &compactionBudgetLLM{firstMainToolCalls: []agent.ToolCall{{
		ID: "post-compaction-tool", Function: agent.ToolCallFunction{Name: "echo", Arguments: `{"input":"x"}`},
	}}}
	env := newCompactionTestEnv(t, llm, echoTool(t), []string{"echo"}, 24000, 1024)
	seedHistory(t, env.mysqlFake, defaultSessionID, 120)
	for i := range env.mysqlFake.messages[defaultSessionID] {
		env.mysqlFake.messages[defaultSessionID][i].Content = strings.Repeat("history detail ", 30)
	}
	token := authToken(t, defaultUserID)
	w := env.postMessage(`{"content":"current task"}`, token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, env.mysqlFake.compactionsFor(defaultSessionID), 1, w.Body.String())

	entries, err := env.redisSvc.ListRuntimeStatus(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, int64(2), entries[0].Seq, "post-compaction tool status starts at sequence two")
	assert.Equal(t, agent.RuntimeStatusToolBudget, entries[0].Kind)

	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"second task"}`, token).Code)
	entries, err = env.redisSvc.ListRuntimeStatus(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, []int64{2, 3}, []int64{entries[0].Seq, entries[1].Seq},
		"the next turn must continue from the real log tail rather than the entry count")

	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"third task"}`, token).Code)
	_, _, requests := llm.snapshot()
	mainRequests := make([]agent.LLMRequest, 0, len(requests))
	for _, req := range nonTitleRequests(requests) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "maintaining the checkpoint") {
			continue
		}
		mainRequests = append(mainRequests, req)
	}
	require.Len(t, mainRequests, 4)
	second := mainRequests[2].Messages
	third := mainRequests[3].Messages
	require.Greater(t, len(third), len(second))
	assert.Equal(t, second, third[:len(second)],
		"a stable checkpoint and valid sequence must not invalidate the whole status log")
}

func TestRuntimeStatus_StableCompactionCheckpointReplaysStatus(t *testing.T) {
	llm := newScriptedLLMClient(
		scriptedLLMResponse{content: "answer one", finishReason: "stop"},
		scriptedLLMResponse{content: "answer two", finishReason: "stop"},
	)
	env := newCompactionTestEnv(t, llm, tool.NewRegistry(), nil, 100000, 1000)
	seedHistory(t, env.mysqlFake, defaultSessionID, 2)
	rows, err := env.mysqlFake.ListMessages(context.Background(), defaultSessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	boundary := rows[len(rows)-1]
	require.NoError(t, env.mysqlFake.InsertCompaction(context.Background(), model.ContextCompaction{
		SessionID: defaultSessionID, UserID: defaultUserID, Content: "stable checkpoint",
		BoundaryMsgTime: boundary.MsgTime, BoundaryMsgIndex: boundary.MsgIndex, BoundaryMsgID: boundary.ID,
	}))
	require.NoError(t, env.mysqlFake.UpdateSessionCompacted(context.Background(), defaultSessionID))
	token := authToken(t, "")

	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question one"}`, token).Code)
	require.Equal(t, http.StatusOK, env.postMessage(`{"content":"question two"}`, token).Code)
	requests := nonTitleRequests(llm.requests())
	require.Len(t, requests, 2)
	first := requests[0].Messages
	second := requests[1].Messages
	require.Greater(t, len(second), len(first))
	assert.Equal(t, first, second[:len(first)],
		"a permanent compacted flag must not clear status when the checkpoint is unchanged")
	assert.Len(t, env.mysqlFake.compactionsFor(defaultSessionID), 1,
		"this regression must not trigger a second compaction")
}

func nonTitleRequests(requests []agent.LLMRequest) []agent.LLMRequest {
	out := make([]agent.LLMRequest, 0, len(requests))
	for _, request := range requests {
		if request.Model != scriptedTitleModel {
			out = append(out, request)
		}
	}
	return out
}
