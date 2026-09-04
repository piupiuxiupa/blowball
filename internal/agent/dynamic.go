package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// SubAgentSnapshotStore persists and loads authoritative sub-agent histories.
// It is intentionally small enough for MySQL and integration fakes to implement
// without importing the agent package.
type SubAgentSnapshotStore interface {
	UpsertSubAgentRun(ctx context.Context, run model.SubAgentRun) error
	GetSubAgentRun(ctx context.Context, sessionID, instanceID string) (model.SubAgentRun, bool, error)
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
// budgets, invocation metadata, construction, resume loading, and snapshot
// writes; parents supply only their own scope.
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

	instanceID := args.ResumeAgentID
	parentID := parent.instanceID
	depth := parent.depth + 1
	var history []Message
	resumedLabel := ""
	if args.ResumeAgentID != "" {
		snapshot, messages, err := c.loadSnapshot(ctx, args.ResumeAgentID)
		if err != nil {
			streamAgentError(hub, ctx, parent.agentName, err.Error(), "bad_args")
			return toolResult{content: err.Error(), isError: true}
		}
		instanceID = snapshot.AgentInstanceID
		parentID = snapshot.ParentInstanceID
		depth = snapshot.Depth
		history = messages
		// The persisted system message is authoritative even when a preset or
		// changed deployment default is supplied on the resume call.
		prompt = systemPromptFromSnapshot(messages, c.cfg.SystemPrompt)
		if len(history) > 0 && history[0].Role == "system" {
			history = history[1:] // Run prepends the resumed system prompt itself.
		}
		// Instance labels are stable across resume so usage.by_agent keeps one key.
		resumedLabel = snapshot.Name
	}

	childDepth := depth
	if args.ResumeAgentID == "" {
		childDepth = parent.depth + 1
	}
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

	if instanceID == "" {
		instanceID = newAgentInstanceID()
	}
	if strings.TrimSpace(args.Name) == "" {
		args.Name = "subagent-" + shortInstanceID(instanceID)
	}
	label := resumedLabel
	if label == "" {
		label = instanceLabel(args.Name, instanceID)
	}

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
		return toolResult{content: appendSpawnResult(msg, instanceID, SubAgentStatusError), isError: true, subAgentID: instanceID, subAgentName: label}
	}

	messages := append(append([]Message(nil), history...), Message{Role: "user", Content: buildSubAgentUserMessage(SpawnToolArgs{
		Task: args.Task, Context: args.Context, Name: args.Name,
	})})
	runHub := stream.TaggedWithAgentRun(hub, instanceID, tc.ID)

	content, usage, err := c.runWithRetry(ctx, sub, messages, runHub, retryBudget, hub)
	status := SubAgentStatusCompleted
	switch {
	case err != nil:
		status = SubAgentStatusError
	case subHitCap(sub):
		status = SubAgentStatusCapped
	}
	c.saveSnapshot(ctx, sub, snapshotData{
		instanceID: instanceID, parentID: parentID, depth: childDepth,
		name: label, tools: effectiveTools, status: status,
		systemPrompt: prompt,
	})
	if parent.instanceID != "" {
		c.meta.observeNestedUsage(label, ownUsageForSub(sub, usage))
	}

	body := content
	if err != nil {
		body = joinFailure(err, content)
	}
	body = appendSpawnResult(body, instanceID, status)
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
	instanceID, parentID       string
	name, status, systemPrompt string
	depth                      int
	tools                      []string
}

func (c *spawnCoordinator) loadSnapshot(ctx context.Context, instanceID string) (model.SubAgentRun, []Message, error) {
	if c.store == nil {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: resume target %q is not available (snapshot store unavailable)", instanceID)
	}
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: resume target %q is not available (session identity missing)", instanceID)
	}
	run, ok, err := c.store.GetSubAgentRun(ctx, sessionID, instanceID)
	if err != nil {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: load resume target %q: %w", instanceID, err)
	}
	if !ok {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: resume target %q does not exist in this session", instanceID)
	}
	if !run.ResumeEligible {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: resume target %q has an oversized or unreadable snapshot", instanceID)
	}
	var messages []Message
	if err := json.Unmarshal(run.MessagesJSON, &messages); err != nil {
		return model.SubAgentRun{}, nil, fmt.Errorf("spawn_subagent: decode resume target %q: %w", instanceID, err)
	}
	return run, messages, nil
}

func (c *spawnCoordinator) saveSnapshot(ctx context.Context, sub Agent, data snapshotData) {
	if c.store == nil || SessionIDFromContext(ctx) == "" {
		return
	}
	tracker, ok := sub.(interface{ LastRunMessages() []Message })
	if !ok {
		return
	}
	messages := append([]Message{{Role: "system", Content: data.systemPrompt}}, tracker.LastRunMessages()...)
	rawMessages, err := json.Marshal(messages)
	if err != nil {
		logger.FromContext(ctx).Warn("marshal sub-agent snapshot failed", zap.Error(err))
		return
	}
	rawTools, _ := json.Marshal(data.tools)
	run := model.SubAgentRun{
		SessionID:        SessionIDFromContext(ctx),
		AgentInstanceID:  data.instanceID,
		ParentInstanceID: data.parentID,
		Depth:            data.depth,
		Name:             data.name,
		ToolsJSON:        rawTools,
		Status:           data.status,
		ResumeEligible:   len(rawMessages) <= c.cfg.MaxSnapshotBytes,
		MessagesJSON:     rawMessages,
	}
	if err := c.store.UpsertSubAgentRun(ctx, run); err != nil {
		logger.FromContext(ctx).Warn("save sub-agent snapshot failed", zap.Error(err))
	}
}

func systemPromptFromSnapshot(messages []Message, fallback string) string {
	if len(messages) > 0 && messages[0].Role == "system" {
		return messages[0].Content
	}
	return fallback
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

func appendSpawnResult(content, instanceID, status string) string {
	return content + subAgentResultMarker + "agent_id: " + instanceID + "\nstatus: " + status + "\n"
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
