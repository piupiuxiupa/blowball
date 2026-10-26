package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContextCompaction_ToolActivityJSONSurvivesCacheRoundTrip(t *testing.T) {
	want := []byte(`[{"tool_name":"grep","calls":2,"success":2,"errors":0}]`)
	rec := ContextCompaction{ToolActivityJSON: want}

	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got ContextCompaction
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.ToolActivityJSON) != string(want) {
		t.Fatalf("ToolActivityJSON = %q, want %q", got.ToolActivityJSON, want)
	}
}

func TestContextCompactionDecodeRetainedResults(t *testing.T) {
	rec := ContextCompaction{
		RetainedResultsJSON: []byte(`{"call-1":"summary","call-2":"error"}`),
	}
	rec.DecodeRetainedResults()
	assert.Equal(t, map[string]string{"call-1": "summary", "call-2": "error"}, rec.RetainedResults)

	legacy := ContextCompaction{}
	legacy.DecodeRetainedResults()
	assert.Nil(t, legacy.RetainedResults, "legacy nil JSON must remain readable")

	bad := ContextCompaction{RetainedResultsJSON: []byte("{")}
	bad.DecodeRetainedResults()
	assert.Nil(t, bad.RetainedResults, "malformed JSON must degrade without failing the record")
}
