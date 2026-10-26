package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
