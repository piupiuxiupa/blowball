package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractToolActivity_LegacySectionRoundTrip(t *testing.T) {
	content := `## Primary Request and Intent
continue safely

## Tool Activity
- grep: 2 calls, 1 succeeded, 1 failed
  - args={"query":"foo"} outcome=one
  - args={"query":"bar"} outcome=error`

	body, activity := ExtractToolActivity(content)

	assert.NotContains(t, body, "## Tool Activity")
	require.Len(t, activity, 1)
	assert.Equal(t, "grep", activity[0].ToolName)
	assert.Equal(t, 2, activity[0].Calls)
	assert.Equal(t, 1, activity[0].Success)
	assert.Equal(t, 1, activity[0].Errors)
	require.Len(t, activity[0].Examples, 2)

	roundTrip := DecodeToolActivity(MarshalToolActivity(activity))
	assert.Equal(t, activity, roundTrip)
}

func TestMergeToolActivity_RecompactionIsAdditiveAndBounded(t *testing.T) {
	prior := []ToolActivity{{
		ToolName: "grep",
		Calls:    2,
		Success:  1,
		Errors:   1,
		Examples: []string{"old-1", "old-2"},
	}}
	current := []ToolActivity{
		{
			ToolName: "grep",
			Calls:    1,
			Success:  1,
			Examples: []string{"old-2", "new-1", "new-2"},
		},
		{ToolName: "bash", Calls: 1, Success: 1, Examples: []string{"bash-1"}},
	}

	merged := MergeToolActivity(prior, current)

	require.Len(t, merged, 2)
	assert.Equal(t, "grep", merged[0].ToolName)
	assert.Equal(t, 3, merged[0].Calls)
	assert.Equal(t, 2, merged[0].Success)
	assert.Equal(t, 1, merged[0].Errors)
	assert.Equal(t, []string{"old-1", "old-2", "new-1"}, merged[0].Examples)
	assert.Equal(t, 4, TotalToolActivityCalls(merged))
	assert.Equal(t, 1, strings.Count(RenderToolActivityLedger(merged), "- grep:"))
}
