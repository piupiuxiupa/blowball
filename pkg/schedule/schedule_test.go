package schedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAtValidationAndUTC(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	rule := Rule{Type: TypeAt, RunAt: "2026-11-05T09:30:00+08:00", Timezone: "Asia/Shanghai"}
	require.NoError(t, Validate(rule, now, true))
	next, err := Next(rule, now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC), next)

	for name, mutate := range map[string]func(*Rule){
		"past":            func(r *Rule) { r.RunAt = "2026-11-03T00:00:00Z" },
		"no offset":       func(r *Rule) { r.RunAt = "2026-11-05T09:30:00" },
		"unknown zone":    func(r *Rule) { r.Timezone = "Mars/Olympus" },
		"local zone":      func(r *Rule) { r.Timezone = "Local" },
		"cron expression": func(r *Rule) { r.Expression = "* * * * *" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := rule
			mutate(&invalid)
			require.Error(t, Validate(invalid, now, true))
		})
	}
}

func TestCronStructureAndTaskTimezone(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	rule := Rule{Type: TypeCron, Expression: "30 9 * * *", Timezone: "America/New_York"}
	require.NoError(t, Validate(rule, now, true))
	next, err := Next(rule, now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 4, 14, 30, 0, 0, time.UTC), next)

	for _, expression := range []string{
		"0 30 9 * * *", "@daily", "@every 5m", "CRON_TZ=UTC 30 9 * * *", "TZ=UTC 30 9 * * *", "61 9 * * *",
	} {
		t.Run(expression, func(t *testing.T) {
			invalid := rule
			invalid.Expression = expression
			require.Error(t, Validate(invalid, now, true))
		})
	}
}

func TestCronNextIsStrictlyAfterNow(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 30, 0, 0, time.UTC)
	next, err := Next(Rule{Type: TypeCron, Expression: "* * * * *", Timezone: "UTC"}, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute), next)
	require.True(t, next.After(now))
}

func TestCronDSTSkipsNonexistentLocalTime(t *testing.T) {
	rule := Rule{Type: TypeCron, Expression: "30 2 * * *", Timezone: "America/New_York"}
	after := time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC) // 01:00 EST; 02:30 does not exist.
	next, err := Next(rule, after)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC), next)
}

func TestCronDSTRepeatedLocalTimeHasDistinctUTCSlots(t *testing.T) {
	rule := Rule{Type: TypeCron, Expression: "30 1 * * *", Timezone: "America/New_York"}
	after := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	first, err := Next(rule, after)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), first)
	second, err := Next(rule, first)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), second)
	require.NotEqual(t, first, second)
}
