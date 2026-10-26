package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/tokens"
)

const (
	toolSummaryOutcomeRunes  = 900
	toolSummaryFallbackRunes = 480
	toolResultExternalMinEst = 1600
)

// ToolResultSummary is the deterministic representation of one tool result in
// the retained model tail. Full output remains in the persisted tool_calls row
// (and may optionally be materialized into a workspace temp file); the summary
// keeps enough facts for a continuation to reason without replaying the payload.
type ToolResultSummary struct {
	ToolName          string `json:"tool_name"`
	ToolCallID        string `json:"tool_call_id"`
	Status            string `json:"status"`
	ExitCode          *int   `json:"exit_code,omitempty"`
	Truncated         bool   `json:"truncated"`
	OriginalBytes     int    `json:"original_bytes"`
	OriginalTokens    int    `json:"original_token_estimate"`
	Outcome           string `json:"outcome"`
	RetrievalRef      string `json:"retrieval_ref,omitempty"`
	RetrievalHint     string `json:"retrieval_hint,omitempty"`
	RetrievalFailNote string `json:"retrieval_fail_note,omitempty"`
}

// SummarizeToolResult renders a bounded, deterministic result body suitable for
// a retained role=tool message. call supplies the original arguments/name; full
// is the complete result body stored for the call.
func SummarizeToolResult(call agent.ToolCall, full string, maxTokens int) string {
	return summarizeToolResult(call, full, maxTokens, "", "")
}

func summarizeToolResult(
	call agent.ToolCall, full string, maxTokens int, retrievalRef, retrievalFailNote string,
) string {
	if full == "" {
		full = "{}"
	}
	summary := ToolResultSummary{
		ToolName:       call.Function.Name,
		ToolCallID:     call.ID,
		Status:         "success",
		OriginalBytes:  len([]byte(full)),
		OriginalTokens: tokens.Estimate(full),
	}
	if maxTokens <= 0 {
		maxTokens = 400
	}

	var envelope struct {
		Status int             `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	var fields map[string]json.RawMessage
	isEnvelope := json.Unmarshal([]byte(full), &fields) == nil
	if isEnvelope {
		_, isEnvelope = fields["status"]
	}
	if isEnvelope {
		isEnvelope = json.Unmarshal([]byte(full), &envelope) == nil
	}
	if isEnvelope && envelope.Status != 0 {
		summary.Status = "error"
	}
	if isEnvelope {
		summary.Outcome = summarizeToolOutcome(call.Function, envelope.Result, envelope.Error, maxTokens)
		if call.Function.Name == "executor_bash" || call.Function.Name == "bash" {
			var r struct {
				ExitCode  int  `json:"exit_code"`
				Truncated bool `json:"truncated"`
			}
			if json.Unmarshal(envelope.Result, &r) == nil {
				summary.ExitCode = &r.ExitCode
				summary.Truncated = r.Truncated
				if r.ExitCode != 0 && summary.Status == "success" {
					summary.Status = "error"
				}
			}
		}
	} else {
		summary.Outcome = summarizeToolOutcome(call.Function, json.RawMessage(full), "", maxTokens)
	}
	if summary.Outcome == "" {
		summary.Outcome = "(no textual output; see retrieval reference)"
	}
	summary.Outcome = truncateRunes(summary.Outcome, toolSummaryOutcomeRunes)
	summary.RetrievalRef = retrievalRef
	summary.RetrievalFailNote = retrievalFailNote
	if retrievalRef != "" {
		summary.RetrievalHint = "Read with xizhi_read_file (offset/limit) or search with xizhi_grep."
	}
	summary.Truncated = summary.Truncated || summary.OriginalTokens > maxTokens
	encoded, err := json.Marshal(summary)
	if err != nil {
		return fmt.Sprintf(`{"tool_name":%q,"tool_call_id":%q,"status":%q,"outcome":%q,"original_bytes":%d}`,
			call.Function.Name, call.ID, summary.Status, truncateRunes(summary.Outcome, toolSummaryFallbackRunes), len(full))
	}
	return boundedToolSummaryJSON(summary, encoded, maxTokens)
}

// boundedToolSummaryJSON keeps the summary valid JSON while honoring the byte
// safety cap. The outcome is reduced before any identity or retrieval fields,
// so a large tool body can never truncate away the readable spill path.
func boundedToolSummaryJSON(summary ToolResultSummary, encoded []byte, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 400
	}
	limit := maxTokens * 4
	if len(encoded) <= limit {
		return string(encoded)
	}
	for len([]rune(summary.Outcome)) > 0 {
		runes := []rune(summary.Outcome)
		keep := len(runes) * 3 / 4
		if keep < 16 {
			keep = 0
		}
		summary.Outcome = string(runes[:keep])
		if keep > 0 {
			summary.Outcome += "…"
		}
		encoded, _ = json.Marshal(summary)
		if len(encoded) <= limit {
			return string(encoded)
		}
	}
	return string(encoded)
}

// summarizeToolOutcome prioritizes exact errors/identifiers and falls back to
// a deterministic head/tail excerpt. No LLM is called here; this function is
// replayed for every compacted request and must remain stable.
func summarizeToolOutcome(fn agent.ToolCallFunction, result json.RawMessage, errText string, maxTokens int) string {
	name := fn.Name
	args := strings.TrimSpace(fn.Arguments)
	raw := strings.TrimSpace(string(result))
	if errText != "" {
		return withToolIdentity(name, args, "ERROR: "+errText)
	}

	switch {
	case strings.Contains(name, "bash"), strings.Contains(name, "executor"):
		var r struct {
			Output    string `json:"output"`
			ExitCode  int    `json:"exit_code"`
			Truncated bool   `json:"truncated"`
		}
		if json.Unmarshal(result, &r) == nil {
			return withToolIdentity(name, args, fmt.Sprintf("exit_code=%d truncated=%v\n%s", r.ExitCode, r.Truncated, boundedHeadTail(r.Output, maxTokens)))
		}
	case strings.Contains(name, "fetch"):
		var r struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			Truncated bool   `json:"truncated"`
		}
		if json.Unmarshal(result, &r) == nil {
			u := r.URL
			if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
				u = parsed.Host + parsed.Path
			}
			return withToolIdentity(name, args, fmt.Sprintf("url=%s title=%s truncated=%v\n%s", u, r.Title, r.Truncated, boundedHeadTail(r.Body, maxTokens)))
		}
	case strings.Contains(name, "read_file"), strings.Contains(name, "grep"), strings.Contains(name, "glob"), strings.Contains(name, "find"):
		var r struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Matches []any  `json:"matches"`
			Total   int    `json:"total"`
		}
		if json.Unmarshal(result, &r) == nil {
			if r.Content != "" {
				return withToolIdentity(name, args, fmt.Sprintf("path=%s\n%s", r.Path, boundedHeadTail(r.Content, maxTokens)))
			}
			return withToolIdentity(name, args, fmt.Sprintf("path=%s total=%d matches=%d", r.Path, r.Total, len(r.Matches)))
		}
	case name == "recall_memories":
		return truncateRunes(fullOrRaw(result), maxTokens*3)
	}
	return withToolIdentity(name, args, boundedHeadTail(raw, maxTokens))
}

func withToolIdentity(name, args, body string) string {
	head := name
	if args != "" && args != "{}" {
		head += "(" + truncateRunes(args, 180) + ")"
	}
	return truncateRunes(head+":\n"+body, toolSummaryOutcomeRunes)
}

func fullOrRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func boundedHeadTail(s string, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 400
	}
	limit := maxTokens * 3
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	return string(r[:head]) + "\n... [tool result condensed] ...\n" + string(r[len(r)-tail:])
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// ToolActivity is the bounded ledger entry for tool calls that move into the
// checkpoint (outside the retained tail).
type ToolActivity struct {
	ToolName string   `json:"tool_name"`
	Calls    int      `json:"calls"`
	Success  int      `json:"success"`
	Errors   int      `json:"errors"`
	Examples []string `json:"examples,omitempty"`
}

// RenderToolActivity consolidates a set of calls/results into one deterministic
// checkpoint section. The containing checkpoint is bounded by the caller's
// budget planner; examples are capped per tool.
func RenderToolActivity(calls []agent.ToolCall, results map[string]string) string {
	byTool := make(map[string]*ToolActivity)
	order := []string{}
	for _, call := range calls {
		entry := byTool[call.Function.Name]
		if entry == nil {
			entry = &ToolActivity{ToolName: call.Function.Name}
			byTool[call.Function.Name] = entry
			order = append(order, call.Function.Name)
		}
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
	var b strings.Builder
	for _, name := range order {
		a := byTool[name]
		fmt.Fprintf(&b, "- %s: %d calls, %d succeeded, %d failed\n", a.ToolName, a.Calls, a.Success, a.Errors)
		for _, ex := range a.Examples {
			if ex != "" {
				fmt.Fprintf(&b, "  - %s\n", strings.ReplaceAll(ex, "\n", " "))
			}
		}
	}
	return strings.TrimSpace(b.String())
}
