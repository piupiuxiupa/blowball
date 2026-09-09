package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"go.uber.org/zap"
)

// Spawn result statuses and the machine-readable suffix written to the parent's
// role="tool" message.
const (
	SubAgentStatusCompleted = model.SubAgentStatusCompleted
	SubAgentStatusCapped    = model.SubAgentStatusCapped
	SubAgentStatusError     = model.SubAgentStatusError

	subAgentResultMarker = "\n\n--- subagent_result ---\n"
)

// SubAgentSnapshotStore persists authoritative sub-agent run deltas. It is
// intentionally small enough for MySQL and integration fakes to implement
// without importing the agent package; model-driven snapshot loading was
// removed with unique fresh dispatch, so only the write path remains here.
type SubAgentSnapshotStore interface {
	AppendSubAgentRun(ctx context.Context, instance model.SubAgentInstance, run model.SubAgentRun) error
}

// SubAgentSpec is the resolved, per-run construction inputs for the generic
// sub-agent implementation. It is deliberately free of model/effort fields:
// those remain turn-level ModelOverride values.
type SubAgentSpec struct {
	InstanceID       string
	Name             string
	SystemPrompt     string
	Tools            []string
	ToolRegistry     *tool.Registry
	MaxRounds        int
	Depth            int
	ParentInstanceID string
	Coordinator      *spawnCoordinator
	Retry            config.AgentRetryConfig
	OutputSchema     string
	Turn             ModelOverride
}

// spawnParent identifies the agent making a spawn and the authorization scope
// its children may narrow from.
type spawnParent struct {
	agentName  string
	instanceID string
	depth      int
	registry   *tool.Registry
	toolNames  []string
}

// spawnCoordinator is shared by every dispatcher in one turn. It owns tree
// budgets, invocation metadata, construction, and per-run snapshot writes;
// parents supply only their own scope.
type spawnCoordinator struct {
	cfg           config.SubAgentConfig
	factory       SubAgentFactory
	budget        *treeBudget
	store         SubAgentSnapshotStore
	meta          *turnMeta
	waitTime      time.Duration
	rootRegistry  *tool.Registry
	rootToolNames []string
}

func newSpawnCoordinator(cfg config.SubAgentConfig, factory SubAgentFactory, store SubAgentSnapshotStore, meta *turnMeta) *spawnCoordinator {
	if meta == nil {
		meta = newTurnMeta()
	}
	return &spawnCoordinator{
		cfg:      cfg,
		factory:  factory,
		budget:   newTreeBudget(cfg.MaxDepth, cfg.MaxConcurrent, cfg.MaxTotalPerTurn, 120*time.Second),
		store:    store,
		meta:     meta,
		waitTime: 120 * time.Second,
	}
}

// dispatch validates and executes one spawn_subagent call. Bad arguments and
// budget exhaustion become error tool results; they never abort the caller's
// agent loop.
func (c *spawnCoordinator) dispatch(ctx context.Context, tc ToolCall, hub stream.EventHub, retryBudget *retryBudget, parent spawnParent) toolResult {
	args, result := parseSpawnArgs(tc)
	if result.isError {
		streamAgentError(hub, ctx, parent.agentName, result.content, "bad_args")
		return result
	}

	requestedTools, prompt, result := c.resolveTemplate(args)
	if result.isError {
		streamAgentError(hub, ctx, parent.agentName, result.content, "bad_args")
		return result
	}

	instanceID := newAgentInstanceID()
	parentID := parent.instanceID
	childDepth := parent.depth + 1
	if childDepth > c.cfg.MaxDepth {
		msg := fmt.Sprintf("spawn_subagent: depth budget exhausted (max %d)", c.cfg.MaxDepth)
		streamAgentError(hub, ctx, parent.agentName, msg, "budget_exhausted")
		return toolResult{content: msg, isError: true}
	}

	effectiveTools := append([]string(nil), parent.toolNames...)
	if requestedTools != nil {
		effectiveTools = append([]string(nil), requestedTools...)
	}
	scoped, err := c.scopeRegistry(parent.registry, effectiveTools)
	if err != nil {
		msg := fmt.Sprintf("spawn_subagent: invalid tools: %v", err)
		streamAgentError(hub, ctx, parent.agentName, msg, "bad_args")
		return toolResult{content: msg, isError: true}
	}

	if strings.TrimSpace(args.Name) == "" {
		args.Name = "subagent-" + shortInstanceID(instanceID)
	}
	// Every accepted dispatch is a fresh instance; the effective label stays
	// unique even when callers reuse the same short name.
	label := instanceLabel(args.Name, instanceID)

	if err := c.budget.reserveTotal(); err != nil {
		msg := fmt.Sprintf("spawn_subagent: total per-turn budget exhausted (%d)", c.cfg.MaxTotalPerTurn)
		streamAgentError(hub, ctx, parent.agentName, msg, "budget_exhausted")
		return toolResult{content: msg, isError: true}
	}
	if err := c.budget.acquireSlot(ctx, c.waitTime); err != nil {
		c.budget.releaseTotal()
		msg := fmt.Sprintf("spawn_subagent: concurrency wait timed out after %s", c.waitTime)
		streamAgentError(hub, ctx, parent.agentName, msg, "budget_timeout")
		return toolResult{content: msg, isError: true}
	}
	defer c.budget.releaseSlot()

	c.meta.observeInvoke(SubAgentInvocation{AgentInstanceID: instanceID, ParentInstanceID: parentID})

	spec := SubAgentSpec{
		InstanceID:       instanceID,
		Name:             label,
		SystemPrompt:     prompt,
		Tools:            effectiveTools,
		ToolRegistry:     scoped,
		MaxRounds:        c.cfg.MaxRounds,
		Depth:            childDepth,
		ParentInstanceID: parentID,
		Coordinator:      c,
		Retry:            c.cfg.Retry,
		OutputSchema:     c.cfg.OutputSchema,
	}
	sub, err := c.factory(spec)
	if err != nil {
		msg := fmt.Sprintf("build sub-agent %s: %v", instanceID, err)
		streamAgentError(hub, ctx, parent.agentName, msg, "unknown_tool")
		return toolResult{content: appendSpawnResult(msg, SubAgentStatusError), isError: true, subAgentID: instanceID, subAgentName: label}
	}

	messages := []Message{{Role: "user", Content: buildSubAgentUserMessage(SpawnToolArgs{
		Task: args.Task, Context: args.Context, Name: args.Name,
	})}}
	runHub := stream.TaggedWithAgentRun(hub, instanceID, tc.ID)
	startedAt := time.Now().UTC()

	content, usage, err := c.runWithRetry(ctx, sub, messages, runHub, retryBudget, hub)
	status := SubAgentStatusCompleted
	switch {
	case err != nil:
		status = SubAgentStatusError
	case subHitCap(sub):
		status = SubAgentStatusCapped
	}
	// Every dispatch is a fresh instance, so the persisted run is always the
	// instance's first (run_no=1, no predecessor, no pre-existing history).
	// Snapshot writes stay best-effort: failures are logged inside saveSnapshot
	// and never abort the caller's agent loop.
	_ = c.saveSnapshot(ctx, sub, snapshotData{
		instance: model.SubAgentInstance{
			SessionID:        SessionIDFromContext(ctx),
			AgentInstanceID:  instanceID,
			ParentInstanceID: parentID,
			Depth:            childDepth,
			Name:             label,
			SystemPrompt:     prompt,
		},
		previousRunID:    "",
		runNo:            1,
		baseMessageCount: 0,
		preRunMessages:   nil,
		tools:            effectiveTools,
		status:           status,
		runID:            tc.ID,
		startedAt:        startedAt,
	})
	if parent.instanceID != "" {
		c.meta.observeNestedUsage(label, ownUsageForSub(sub, usage))
	}

	body := content
	if err != nil {
		body = joinFailure(err, content)
	}
	body = appendSpawnResult(body, status)
	return toolResult{
		content: body, isError: err != nil, subUsage: &usage,
		subOwnUsage:  ptrToUsage(ownUsageForSub(sub, usage)),
		subAgentName: label, subAgentID: instanceID, subParentID: parentID,
		subCapped: status == SubAgentStatusCapped, subStatus: status,
	}
}

func ownUsageForSub(sub Agent, subtree Usage) Usage {
	if tracker, ok := sub.(interface{ LastRunOwnUsage() Usage }); ok {
		return tracker.LastRunOwnUsage()
	}
	return subtree
}

func ptrToUsage(u Usage) *Usage { return &u }

func parseSpawnArgs(tc ToolCall) (SpawnToolArgs, toolResult) {
	dec := json.NewDecoder(strings.NewReader(tc.Function.Arguments))
	dec.DisallowUnknownFields()
	var args SpawnToolArgs
	if err := dec.Decode(&args); err != nil && !errors.Is(err, context.Canceled) {
		return args, toolResult{content: fmt.Sprintf("parse spawn_subagent args: %v", err), isError: true}
	}
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return args, toolResult{content: "parse spawn_subagent args: trailing JSON values", isError: true}
	}
	if strings.TrimSpace(args.Task) == "" {
		return args, toolResult{content: `spawn_subagent: missing required field "task"`, isError: true}
	}
	return args, toolResult{}
}

func (c *spawnCoordinator) resolveTemplate(args SpawnToolArgs) ([]string, string, toolResult) {
	prompt := c.cfg.SystemPrompt
	var tools []string
	if args.Preset != "" {
		preset, ok := c.cfg.Presets[args.Preset]
		if !ok {
			return nil, "", toolResult{content: fmt.Sprintf("spawn_subagent: unknown preset %q", args.Preset), isError: true}
		}
		if strings.TrimSpace(preset.SystemPrompt) != "" {
			prompt = preset.SystemPrompt
		}
		if preset.Tools != nil {
			tools = append(tools, preset.Tools...)
		}
	}
	if args.Tools != nil {
		tools = append([]string(nil), args.Tools...)
	}
	return tools, prompt, toolResult{}
}

func (c *spawnCoordinator) scopeRegistry(parent *tool.Registry, names []string) (*tool.Registry, error) {
	if parent == nil {
		if len(names) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("no tool registry is available")
	}
	return parent.Scope(names)
}

func (c *spawnCoordinator) runWithRetry(ctx context.Context, sub Agent, messages []Message, runHub stream.EventHub, retryBudget *retryBudget, hub stream.EventHub) (string, Usage, error) {
	content, usage, _, err := sub.Run(ctx, messages, runHub)
	if err == nil || !shouldRetry(sub, err, sub.RetryPolicy(), retryBudget) {
		return content, usage, err
	}
	policy := sub.RetryPolicy()
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = config.DefaultRetryMaxAttempts()
	}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		retryBudget.charge(usage)
		if !retryBudget.allows() {
			return content, usage, err
		}
		if tracker, ok := sub.(ToolCallTracker); ok && tracker.LastRunExecutedTool() {
			return content, usage, err
		}
		hub.SendCtx(ctx, retryErrorEvent(sub.Name(), err))
		select {
		case <-time.After(computeBackoff(policy, attempt)):
		case <-ctx.Done():
			return content, usage, ctx.Err()
		}
		content, usage, _, err = sub.Run(ctx, messages, runHub)
		if err == nil || !isTransientError(err) {
			return content, usage, err
		}
	}
	return content, usage, err
}

type snapshotData struct {
	instance                model.SubAgentInstance
	runID, previousRunID    string
	runNo, baseMessageCount int
	status                  string
	preRunMessages          []Message
	tools                   []string
	startedAt               time.Time
}

func (c *spawnCoordinator) saveSnapshot(ctx context.Context, sub Agent, data snapshotData) error {
	if c.store == nil || SessionIDFromContext(ctx) == "" {
		return nil
	}
	tracker, ok := sub.(interface{ LastRunMessages() []Message })
	if !ok {
		return nil
	}
	fullMessages := tracker.LastRunMessages()
	if len(fullMessages) < len(data.preRunMessages) {
		return fmt.Errorf("sub-agent run dropped persisted history: got %d messages, want at least %d", len(fullMessages), len(data.preRunMessages))
	}
	if len(data.preRunMessages) > 0 && !reflect.DeepEqual(fullMessages[:len(data.preRunMessages)], data.preRunMessages) {
		return fmt.Errorf("sub-agent run mutated persisted history before its new delta")
	}
	delta := fullMessages[len(data.preRunMessages):]
	rawMessages, err := json.Marshal(delta)
	if err != nil {
		logger.FromContext(ctx).Warn("marshal sub-agent snapshot failed", zap.Error(err))
		return err
	}
	fullContext := append([]Message{{Role: "system", Content: data.instance.SystemPrompt}}, fullMessages...)
	rawFullContext, err := json.Marshal(fullContext)
	if err != nil {
		logger.FromContext(ctx).Warn("marshal sub-agent full context failed", zap.Error(err))
		return err
	}
	rawTools, _ := json.Marshal(data.tools)
	instance := data.instance
	instance.ToolsJSON = rawTools
	run := model.SubAgentRun{
		SessionID:           instance.SessionID,
		AgentInstanceID:     instance.AgentInstanceID,
		RunID:               data.runID,
		PreviousRunID:       data.previousRunID,
		RunNo:               data.runNo,
		ParentInstanceID:    instance.ParentInstanceID,
		Depth:               instance.Depth,
		Name:                instance.Name,
		ToolsJSON:           rawTools,
		Status:              data.status,
		SnapshotKind:        model.SubAgentSnapshotDelta,
		ResumeEligible:      len(rawFullContext) <= c.cfg.MaxSnapshotBytes,
		BaseMessageCount:    data.baseMessageCount,
		MessageCount:        len(delta),
		ContextMessageCount: len(fullMessages),
		ContextBytes:        len(rawFullContext),
		MessagesJSON:        rawMessages,
		StartedAt:           data.startedAt,
		FinishedAt:          time.Now().UTC(),
	}
	if err := c.store.AppendSubAgentRun(ctx, instance, run); err != nil {
		logger.FromContext(ctx).Warn("save sub-agent snapshot failed", zap.Error(err))
		return err
	}
	return nil
}

func newAgentInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("w-%d", time.Now().UnixNano())
	}
	return "w-" + hex.EncodeToString(b[:])
}

func shortInstanceID(id string) string {
	return strings.TrimPrefix(id, "w-")
}

func instanceLabel(name, id string) string {
	if strings.TrimSpace(name) == "" {
		return "subagent-" + shortInstanceID(id)
	}
	return strings.TrimSpace(name) + "#" + shortInstanceID(id)
}

// appendSpawnResult renders the model-visible spawn outcome. It exposes only
// the run status: instance identity stays internal so the model can never
// address a past instance (every dispatch is a fresh instance).
func appendSpawnResult(content, status string) string {
	return content + subAgentResultMarker + "status: " + status + "\n"
}

// treeBudget bounds a whole spawn tree. total is reserved before waiting so an
// exhausted turn fails immediately; the slot semaphore preserves each waiting
// ancestor's slot, preventing a child from replacing its parent in the budget.
type treeBudget struct {
	depth    int
	slots    chan struct{}
	total    atomic.Int64
	maxTotal int64
	wait     time.Duration
	waitMu   sync.Mutex
}

func newTreeBudget(depth, concurrent, total int, wait time.Duration) *treeBudget {
	if concurrent <= 0 {
		concurrent = 1
	}
	return &treeBudget{depth: depth, slots: make(chan struct{}, concurrent), maxTotal: int64(total), wait: wait}
}

func (b *treeBudget) reserveTotal() error {
	if b.maxTotal <= 0 {
		b.total.Add(1)
		return nil
	}
	if b.total.Add(1) > b.maxTotal {
		b.total.Add(-1)
		return fmt.Errorf("total spawn budget exhausted")
	}
	return nil
}

func (b *treeBudget) releaseTotal() { b.total.Add(-1) }

func (b *treeBudget) acquireSlot(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		wait = b.wait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case b.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return fmt.Errorf("concurrency slot wait timed out after %s", wait)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *treeBudget) releaseSlot() { <-b.slots }
