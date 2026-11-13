package agent

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanLedger_ValidSnapshotsAndConsecutiveRevisions(t *testing.T) {
	ledger := &planLedger{}

	first, err := ledger.update(updatePlanArgs{
		Steps: []PlanStep{
			{Step: "  Inspect event stream  ", Status: PlanStatusCompleted},
			{Step: "Design plan state", Status: PlanStatusInProgress},
			{Step: "Verify integration", Status: PlanStatusPending},
		},
		Explanation: "  Investigation finished.  ",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, first.Revision)
	assert.Equal(t, []PlanStep{
		{Step: "Inspect event stream", Status: PlanStatusCompleted},
		{Step: "Design plan state", Status: PlanStatusInProgress},
		{Step: "Verify integration", Status: PlanStatusPending},
	}, first.Steps)
	assert.Equal(t, "Investigation finished.", first.Explanation)

	second, err := ledger.update(updatePlanArgs{
		Steps: []PlanStep{{Step: "Ship", Status: PlanStatusInProgress}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, second.Revision)
	assert.Empty(t, second.Explanation)

	current, ok := ledger.current()
	require.True(t, ok)
	assert.Equal(t, second, current)
}

func TestPlanLedger_RejectsInvalidInputWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		args updatePlanArgs
	}{
		{name: "no steps", args: updatePlanArgs{}},
		{name: "empty step", args: updatePlanArgs{Steps: []PlanStep{{Step: "  ", Status: PlanStatusPending}}}},
		{name: "invalid status", args: updatePlanArgs{Steps: []PlanStep{{Step: "x", Status: "blocked"}}}},
		{name: "too many steps", args: updatePlanArgs{Steps: repeatPlanSteps(21)}},
		{name: "oversized step", args: updatePlanArgs{Steps: []PlanStep{{Step: strings.Repeat("x", 1001), Status: PlanStatusPending}}}},
		{name: "oversized explanation", args: updatePlanArgs{
			Steps:       []PlanStep{{Step: "x", Status: PlanStatusPending}},
			Explanation: strings.Repeat("x", 2001),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &planLedger{}
			_, beforeErr := ledger.update(updatePlanArgs{
				Steps: []PlanStep{{Step: "stable", Status: PlanStatusInProgress}},
			})
			require.NoError(t, beforeErr)

			_, err := ledger.update(tt.args)
			require.Error(t, err)
			current, ok := ledger.current()
			require.True(t, ok)
			assert.Equal(t, 1, current.Revision)
			assert.Equal(t, []PlanStep{{Step: "stable", Status: PlanStatusInProgress}}, current.Steps)
		})
	}
}

func TestPlanLedger_IsConcurrencySafe(t *testing.T) {
	ledger := &planLedger{}
	const updates = 32

	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ledger.update(updatePlanArgs{
				Steps: []PlanStep{{Step: "step", Status: PlanStatusPending}},
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	current, ok := ledger.current()
	require.True(t, ok)
	assert.Equal(t, updates, current.Revision)
}

func TestParseUpdatePlanArgs_StrictDecoder(t *testing.T) {
	valid := `{"steps":[{"step":"Inspect","status":"in_progress"}],"explanation":"begin"}`
	args, result := parseUpdatePlanArgs(ToolCall{Function: ToolCallFunction{Arguments: valid}})
	require.False(t, result.isError)
	require.Len(t, args.Steps, 1)
	assert.Equal(t, "Inspect", args.Steps[0].Step)
	assert.Equal(t, PlanStatusInProgress, args.Steps[0].Status)
	assert.Equal(t, "begin", args.Explanation)

	tests := []struct {
		name string
		raw  string
	}{
		{name: "model supplied revision", raw: `{"steps":[],"revision":1}`},
		{name: "trailing json", raw: valid + ` {}`},
		{name: "malformed json", raw: `{`},
		{name: "wrong type", raw: `{"steps":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, result := parseUpdatePlanArgs(ToolCall{Function: ToolCallFunction{Arguments: tt.raw}})
			assert.True(t, result.isError)
			assert.NotEmpty(t, result.content)
			assert.Empty(t, args.Steps)
		})
	}
}

func TestPlanSnapshotCanonicalJSON(t *testing.T) {
	snapshot := PlanSnapshot{
		Revision: 2,
		Steps:    []PlanStep{{Step: "Verify", Status: PlanStatusCompleted}},
	}
	raw, err := snapshot.canonicalJSON()
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	assert.Equal(t, float64(2), decoded["revision"])
	require.Len(t, decoded["steps"], 1)
	assert.NotContains(t, raw, "explanation")
}

func repeatPlanSteps(n int) []PlanStep {
	out := make([]PlanStep, n)
	for i := range out {
		out[i] = PlanStep{Step: "step", Status: PlanStatusPending}
	}
	return out
}
