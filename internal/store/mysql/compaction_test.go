package mysql

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCompactionRecordRow_NullObservationsRemainReadable(t *testing.T) {
	retained, err := json.Marshal(map[string]string{"call-1": "summary"})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	row := compactionRecordRow{
		ID:                      7,
		SessionID:               "session-1",
		UserID:                  "user-1",
		TraceID:                 "trace-1",
		TriggerKind:             "mid_turn",
		TriggerTokens:           80000,
		Content:                 "checkpoint",
		BoundaryMsgTime:         created,
		BoundaryMsgIndex:        3,
		BoundaryMsgID:           42,
		AnchorContent:           sql.NullString{String: "bounded anchor", Valid: true},
		AnchorTruncated:         sql.NullBool{Bool: true, Valid: true},
		ShadowedTokens:          100,
		SummaryModel:            "model-1",
		SummaryPromptTokens:     100,
		SummaryCompletionTokens: 20,
		RetainedResultsJSON:     retained,
		MeasurementBasis:        sql.NullString{},
		PreCompactionTokens:     sql.NullInt64{},
		InputBudgetTokens:       sql.NullInt64{},
		PostCompactionTokens:    sql.NullInt64{},
		RetainedTailMessages:    sql.NullInt64{},
		SummaryStrategy:         sql.NullString{},
		ToolSummaryCount:        sql.NullInt64{},
		ToolActivityCount:       sql.NullInt64{},
		CreateTime:              created,
	}

	rec := row.model()
	rec.DecodeRetainedResults()

	assert.Equal(t, int64(7), rec.ID)
	assert.Equal(t, "bounded anchor", rec.AnchorContent)
	assert.True(t, rec.AnchorTruncated)
	assert.Empty(t, rec.MeasurementBasis)
	assert.Zero(t, rec.PreCompactionTokens)
	assert.Zero(t, rec.InputBudgetTokens)
	assert.Zero(t, rec.PostCompactionTokens)
	assert.Equal(t, "summary", rec.RetainedResults["call-1"])
}
