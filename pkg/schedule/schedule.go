// Package schedule parses and evaluates the structured time rules accepted by
// scheduled agent tasks. It has no persistence or runtime dependencies.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	TypeAt   = "at"
	TypeCron = "cron"
)

// Rule is the API/storage shape of one schedule. RunAt is RFC3339 for at;
// Expression is a standard five-field cron expression for cron. Timezone is an
// explicit IANA location for both kinds.
type Rule struct {
	Type       string `json:"type"`
	RunAt      string `json:"run_at,omitempty"`
	Expression string `json:"expression,omitempty"`
	Timezone   string `json:"timezone"`
}

var standardCron = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Validate checks the rule's structure. When atMustBeFuture is true (create or
// an explicit schedule update), an at instant must be strictly after now.
func Validate(rule Rule, now time.Time, atMustBeFuture bool) error {
	rule.Timezone = strings.TrimSpace(rule.Timezone)
	if rule.Timezone == "" || rule.Timezone == "Local" {
		return fmt.Errorf("timezone must be an explicit IANA location")
	}
	if _, err := time.LoadLocation(rule.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", rule.Timezone, err)
	}

	switch rule.Type {
	case TypeAt:
		if strings.TrimSpace(rule.Expression) != "" {
			return fmt.Errorf("at schedule must not include expression")
		}
		runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(rule.RunAt))
		if err != nil {
			return fmt.Errorf("run_at must be RFC3339 with a timezone offset: %w", err)
		}
		if atMustBeFuture && !runAt.After(now) {
			return fmt.Errorf("run_at must be in the future")
		}
	case TypeCron:
		if strings.TrimSpace(rule.RunAt) != "" {
			return fmt.Errorf("cron schedule must not include run_at")
		}
		expression := strings.TrimSpace(rule.Expression)
		if len(strings.Fields(expression)) != 5 {
			return fmt.Errorf("cron expression must contain exactly five fields")
		}
		if strings.HasPrefix(expression, "@") || strings.HasPrefix(expression, "CRON_TZ=") || strings.HasPrefix(expression, "TZ=") {
			return fmt.Errorf("cron macros, every rules, and expression timezone prefixes are not supported")
		}
		if _, err := standardCron.Parse(expression); err != nil {
			return fmt.Errorf("invalid cron expression: %w", err)
		}
	default:
		return fmt.Errorf("schedule type must be %q or %q", TypeAt, TypeCron)
	}
	return nil
}

// Next returns the rule's next UTC slot. at returns its single instant; cron
// always computes a slot strictly after the supplied instant in the rule's
// own IANA location, independent of the process-local timezone.
func Next(rule Rule, after time.Time) (time.Time, error) {
	if err := Validate(rule, after, false); err != nil {
		return time.Time{}, err
	}
	if rule.Type == TypeAt {
		runAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(rule.RunAt))
		return runAt.UTC(), nil
	}
	parsed, err := standardCron.Parse(strings.TrimSpace(rule.Expression))
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	loc, _ := time.LoadLocation(strings.TrimSpace(rule.Timezone))
	next := parsed.Next(after.In(loc)).UTC()
	if !next.After(after) {
		return time.Time{}, fmt.Errorf("cron next slot %s is not after %s", next, after)
	}
	return next, nil
}
