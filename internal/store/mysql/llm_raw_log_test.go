package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// setupRawLogTestStore opens a connection and creates the llm_raw_log table.
// Tests are skipped when MYSQL_TEST_DSN is not set (same convention as the
// messages store tests).
func setupRawLogTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL-backed llm_raw_log store test")
	}

	store, err := New(dsn)
	require.NoError(t, err)

	cleanup := func() {
		_, _ = store.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS llm_raw_log")
		_ = store.Close()
	}

	// Column layout matches migration 011_llm_raw_log.sql.
	_, err = store.db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS llm_raw_log (
			id            BIGINT       NOT NULL AUTO_INCREMENT,
			call_id       CHAR(36)     NOT NULL,
			seq           INT          NOT NULL,
			frame_index   INT          NOT NULL DEFAULT 0,
			trace_id      CHAR(36)     NOT NULL,
			session_id    CHAR(36)     NOT NULL,
			user_id       CHAR(36)     NOT NULL,
			agent         VARCHAR(32)  NOT NULL,
			kind          VARCHAR(16)  NOT NULL,
			model         VARCHAR(128) NOT NULL,
			finish_reason VARCHAR(32)  NOT NULL DEFAULT '',
			http_status   SMALLINT     NOT NULL DEFAULT 0,
			duration_ms   INT          NOT NULL DEFAULT 0,
			raw           MEDIUMTEXT   NOT NULL,
			msg_time      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			update_time   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
			PRIMARY KEY (id),
			KEY idx_llm_raw_session_seq (session_id, seq, frame_index),
			KEY idx_llm_raw_trace_seq (trace_id, seq),
			KEY idx_llm_raw_time (msg_time)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
	`)
	require.NoError(t, err)

	return store, cleanup
}

func rawLogRow(callID string, seq, frameIndex int, kind, raw string) model.LLMRawLog {
	return model.LLMRawLog{
		CallID:       callID,
		Seq:          seq,
		FrameIndex:   frameIndex,
		TraceID:      "trace-1",
		SessionID:    "sess-1",
		UserID:       "user-1",
		Agent:        "Confucius",
		Kind:         kind,
		Model:        "gpt-test",
		FinishReason: "stop",
		HTTPStatus:   0,
		DurationMS:   42,
		Raw:          raw,
		MsgTime:      time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestAppendRawLogs_BatchInsert(t *testing.T) {
	store, cleanup := setupRawLogTestStore(t)
	defer cleanup()
	ctx := context.Background()

	logs := []model.LLMRawLog{
		rawLogRow("call-a", 1, 0, model.RawKindRequest, `{"messages":[]}`),
		rawLogRow("call-a", 1, 1, model.RawKindChunk, `{"id":"c","choices":[{"delta":{"content":"He"}}]}`),
		rawLogRow("call-a", 1, 2, model.RawKindChunk, `{"id":"c","choices":[{"delta":{"content":"llo"}}]}`),
		rawLogRow("call-a", 1, 3, model.RawKindResponse, `{"choices":[]}`),
		rawLogRow("call-b", 2, 0, model.RawKindError, `{"error":"bad"}`),
	}
	require.NoError(t, store.AppendRawLogs(ctx, logs))

	var rows []model.LLMRawLog
	err := store.db.SelectContext(ctx, &rows,
		`SELECT id, call_id, seq, frame_index, trace_id, session_id, user_id, agent, kind, model,
		        finish_reason, http_status, duration_ms, raw, msg_time
		 FROM llm_raw_log ORDER BY seq, frame_index`)
	require.NoError(t, err)
	require.Len(t, rows, 5)

	assert.Equal(t, "call-a", rows[0].CallID)
	assert.Equal(t, model.RawKindRequest, rows[0].Kind)
	assert.Equal(t, 0, rows[0].FrameIndex)
	assert.Equal(t, 1, rows[0].Seq)
	assert.Equal(t, model.RawKindChunk, rows[1].Kind)
	assert.Equal(t, 1, rows[1].FrameIndex)
	assert.Equal(t, "stop", rows[0].FinishReason)
	assert.Equal(t, int64(42), rows[0].DurationMS)
	assert.Equal(t, `{"choices":[]}`, rows[3].Raw)
	assert.Equal(t, model.RawKindError, rows[4].Kind)

	// Empty batch is a no-op, not an error.
	require.NoError(t, store.AppendRawLogs(ctx, nil))
}

func TestAppendRawLogs_OversizedRowIsolation(t *testing.T) {
	store, cleanup := setupRawLogTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// A raw payload beyond MEDIUMTEXT's 16MB ceiling fails the multi-value
	// batch; the per-row fallback must keep the two healthy rows and drop
	// only the oversized one.
	oversized := make([]byte, 16*1024*1024+1024)
	for i := range oversized {
		oversized[i] = 'x'
	}
	logs := []model.LLMRawLog{
		rawLogRow("call-ok-1", 1, 0, model.RawKindRequest, "{}"),
		rawLogRow("call-big", 2, 0, model.RawKindResponse, string(oversized)),
		rawLogRow("call-ok-2", 3, 0, model.RawKindRequest, "{}"),
	}
	require.NoError(t, store.AppendRawLogs(ctx, logs))

	var rows []model.LLMRawLog
	err := store.db.SelectContext(ctx, &rows,
		`SELECT call_id FROM llm_raw_log ORDER BY id`)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "call-ok-1", rows[0].CallID)
	assert.Equal(t, "call-ok-2", rows[1].CallID)
}
