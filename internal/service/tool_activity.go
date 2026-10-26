package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/lush/blowball/internal/agent"
)

// ToolActivity is the bounded ledger entry for tool calls that move into the
// checkpoint (outside the retained tail).
type ToolActivity struct {
	ToolName string   `json:"tool_name"`
	Calls    int      `json:"calls"`
	Success  int      `json:"success"`
	Errors   int      `json:"errors"`
	Examples []string `json:"examples,omitempty"`
}

func BuildToolActivity(calls []agent.ToolCall, results map[string]string) []ToolActivity {
	ledger := make([]ToolActivity, 0)
	index := make(map[string]int)
	for _, call := range calls {
		entryIndex, ok := index[call.Function.Name]
		if !ok {
			entryIndex = len(ledger)
			index[call.Function.Name] = entryIndex
			ledger = append(ledger, ToolActivity{ToolName: call.Function.Name})
		}
		entry := &ledger[entryIndex]
		entry.Calls++
		result := results[call.ID]
		summary := SummarizeToolResult(call, result, 80)
		if strings.Contains(summary, `"status":"error"`) {
			entry.Errors++
		} else {
			entry.Success++
		}
		if len(entry.Examples) < 3 {
			args := truncateRunes(strings.TrimSpace(call.Function.Arguments), 120)
			outcome := truncateRunes(strings.TrimSpace(result), 160)
			entry.Examples = append(entry.Examples, fmt.Sprintf("args=%s outcome=%s", args, outcome))
		}
	}
	return ledger
}

func MergeToolActivity(groups ...[]ToolActivity) []ToolActivity {
	merged := make([]ToolActivity, 0)
	index := make(map[string]int)
	for _, group := range groups {
		for _, source := range group {
			if strings.TrimSpace(source.ToolName) == "" {
				continue
			}
			entryIndex, ok := index[source.ToolName]
			if !ok {
				entryIndex = len(merged)
				index[source.ToolName] = entryIndex
				merged = append(merged, ToolActivity{ToolName: source.ToolName})
			}
			entry := &merged[entryIndex]
			entry.Calls += source.Calls
			entry.Success += source.Success
			entry.Errors += source.Errors
			for _, example := range source.Examples {
				example = strings.TrimSpace(example)
				if example == "" || containsString(entry.Examples, example) {
					continue
				}
				if len(entry.Examples) < 3 {
					entry.Examples = append(entry.Examples, example)
				}
			}
		}
	}
	return merged
}

func RenderToolActivity(calls []agent.ToolCall, results map[string]string) string {
	return RenderToolActivityLedger(BuildToolActivity(calls, results))
}

func RenderToolActivityLedger(ledger []ToolActivity) string {
	var b strings.Builder
	for _, activity := range ledger {
		if activity.Calls == 0 || strings.TrimSpace(activity.ToolName) == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %d calls, %d succeeded, %d failed\n",
			activity.ToolName, activity.Calls, activity.Success, activity.Errors)
		for _, example := range activity.Examples[:minInt(len(activity.Examples), 3)] {
			if example != "" {
				fmt.Fprintf(&b, "  - %s\n", strings.ReplaceAll(example, "\n", " "))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func TotalToolActivityCalls(ledger []ToolActivity) int {
	total := 0
	for _, activity := range ledger {
		total += activity.Calls
	}
	return total
}

func MarshalToolActivity(ledger []ToolActivity) []byte {
	if len(ledger) == 0 {
		return nil
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		return nil
	}
	return raw
}

func DecodeToolActivity(raw []byte) []ToolActivity {
	if len(raw) == 0 {
		return nil
	}
	var ledger []ToolActivity
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil
	}
	return ledger
}

// ExtractToolActivity removes every ## Tool Activity section from a checkpoint
// and parses it as the legacy text ledger when structured JSON is unavailable.
func ExtractToolActivity(content string) (string, []ToolActivity) {
	body, sections := splitToolActivitySections(content)
	return body, parseToolActivitySections(sections)
}

func splitToolActivitySections(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	body := make([]string, 0, len(lines))
	sections := make([]string, 0)
	var current []string
	flush := func() {
		if current != nil {
			sections = append(sections, strings.Join(current, "\n"))
			current = nil
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "## Tool Activity" {
			flush()
			current = make([]string, 0)
			continue
		}
		if current != nil && strings.HasPrefix(trimmed, "## ") {
			flush()
		}
		if current != nil {
			current = append(current, line)
			continue
		}
		body = append(body, line)
	}
	flush()
	return strings.TrimSpace(strings.Join(body, "\n")), sections
}

func parseToolActivitySections(sections []string) []ToolActivity {
	groups := make([][]ToolActivity, 0, len(sections))
	for _, section := range sections {
		groups = append(groups, parseToolActivitySection(section))
	}
	return MergeToolActivity(groups...)
}

func parseToolActivitySection(section string) []ToolActivity {
	var ledger []ToolActivity
	current := -1
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			text := strings.TrimPrefix(trimmed, "- ")
			colon := strings.LastIndex(text, ": ")
			if colon >= 0 {
				name := strings.TrimSpace(text[:colon])
				calls, success, failures, ok := parseToolActivityCounts(text[colon+2:])
				if ok && name != "" {
					ledger = append(ledger, ToolActivity{
						ToolName: name,
						Calls:    calls,
						Success:  success,
						Errors:   failures,
					})
					current = len(ledger) - 1
					continue
				}
			}
		}
		if current >= 0 && strings.HasPrefix(line, "  - ") && len(ledger[current].Examples) < 3 {
			example := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if example != "" {
				ledger[current].Examples = append(ledger[current].Examples, example)
			}
		}
	}
	return ledger
}

func parseToolActivityCounts(text string) (calls, success, failures int, ok bool) {
	parts := strings.Split(text, ", ")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	parse := func(part, suffix string) (int, bool) {
		if !strings.HasSuffix(part, suffix) {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSuffix(part, suffix))
		return n, err == nil
	}
	calls, okCalls := parse(parts[0], " calls")
	success, okSuccess := parse(parts[1], " succeeded")
	failures, okFailures := parse(parts[2], " failed")
	return calls, success, failures, okCalls && okSuccess && okFailures
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
