package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// Semantic plan step status values. Multiple steps may be in_progress at once:
// Confucius can dispatch parallel sub-agents in one assistant round.
const (
	PlanStatusPending    = "pending"
	PlanStatusInProgress = "in_progress"
	PlanStatusCompleted  = "completed"
)

// Plan bounds keep malformed model input from becoming an unbounded UI or
// context payload. They are intentionally deployment-independent for V1.
const (
	planMaxSteps            = 20
	planMaxStepRunes        = 1000
	planMaxExplanationRunes = 2000
)

// PlanStep is one normalized semantic work item. Unlike subagent_runs status,
// this status expresses Confucius's judgment rather than a child executor's
// runtime outcome.
type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// PlanSnapshot is the host-authoritative whole-plan state. Revision is assigned
// by planLedger and never accepted from the model.
type PlanSnapshot struct {
	Revision    int        `json:"revision"`
	Steps       []PlanStep `json:"steps"`
	Explanation string     `json:"explanation,omitempty"`
}

// updatePlanArgs is the model-authored input shape. It deliberately has no
// revision field; unknown fields are rejected by the decoder.
type updatePlanArgs struct {
	Steps       []PlanStep `json:"steps"`
	Explanation string     `json:"explanation"`
}

// planLedger owns semantic plan state for one Confucius Run. A fresh Confucius
// instance is built per top-level turn, so its initial revision is always one.
// The mutex keeps the control plane safe even if dispatch is refactored.
type planLedger struct {
	mu       sync.Mutex
	revision int
	snapshot *PlanSnapshot
}

// update validates and normalizes input, assigns the next host revision, and
// replaces the current whole-plan snapshot atomically.
func (l *planLedger) update(input updatePlanArgs) (PlanSnapshot, error) {
	steps, explanation, err := normalizePlanInput(input)
	if err != nil {
		return PlanSnapshot{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.revision++
	out := PlanSnapshot{
		Revision:    l.revision,
		Steps:       steps,
		Explanation: explanation,
	}
	l.snapshot = &out
	return out, nil
}

// current returns a detached copy of the latest snapshot, if any.
func (l *planLedger) current() (PlanSnapshot, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.snapshot == nil {
		return PlanSnapshot{}, false
	}
	out := l.snapshot.copy()
	return out, true
}

func normalizePlanInput(input updatePlanArgs) ([]PlanStep, string, error) {
	if len(input.Steps) == 0 {
		return nil, "", fmt.Errorf("update_plan: at least one step is required")
	}
	if len(input.Steps) > planMaxSteps {
		return nil, "", fmt.Errorf("update_plan: at most %d steps are allowed", planMaxSteps)
	}

	steps := make([]PlanStep, len(input.Steps))
	for i, step := range input.Steps {
		text := strings.TrimSpace(step.Step)
		if text == "" {
			return nil, "", fmt.Errorf("update_plan: step %d must not be empty", i+1)
		}
		if utf8.RuneCountInString(text) > planMaxStepRunes {
			return nil, "", fmt.Errorf("update_plan: step %d exceeds %d characters", i+1, planMaxStepRunes)
		}
		switch step.Status {
		case PlanStatusPending, PlanStatusInProgress, PlanStatusCompleted:
			steps[i] = PlanStep{Step: text, Status: step.Status}
		default:
			return nil, "", fmt.Errorf("update_plan: step %d has invalid status %q", i+1, step.Status)
		}
	}

	explanation := strings.TrimSpace(input.Explanation)
	if utf8.RuneCountInString(explanation) > planMaxExplanationRunes {
		return nil, "", fmt.Errorf("update_plan: explanation exceeds %d characters", planMaxExplanationRunes)
	}
	return steps, explanation, nil
}

func (p PlanSnapshot) copy() PlanSnapshot {
	out := PlanSnapshot{
		Revision:    p.Revision,
		Explanation: p.Explanation,
	}
	if p.Steps != nil {
		out.Steps = append([]PlanStep(nil), p.Steps...)
	}
	return out
}

// canonicalJSON renders the stable tool-result and plan_updated payload.
func (p PlanSnapshot) canonicalJSON() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("update_plan: render canonical snapshot: %w", err)
	}
	return string(b), nil
}

// parseUpdatePlanArgs strictly decodes update_plan arguments. It rejects
// unknown fields (including a model-supplied revision) and trailing JSON.
func parseUpdatePlanArgs(tc ToolCall) (updatePlanArgs, toolResult) {
	dec := json.NewDecoder(strings.NewReader(tc.Function.Arguments))
	dec.DisallowUnknownFields()
	var args updatePlanArgs
	if err := dec.Decode(&args); err != nil {
		args = updatePlanArgs{}
		return args, toolResult{
			content: fmt.Sprintf("parse update_plan args: %v", err),
			isError: true,
		}
	}
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		args = updatePlanArgs{}
		return args, toolResult{
			content: "parse update_plan args: trailing JSON values",
			isError: true,
		}
	}
	return args, toolResult{}
}
