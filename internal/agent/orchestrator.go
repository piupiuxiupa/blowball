package agent

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/xizhi"
	"go.uber.org/zap"
)

// AgentFactory builds a fresh set of agents for a single request. The Chongzhi
// agent binds Xizhi tools against the requesting user's workspace_root, which
// varies per request, so a new Chongzhi (and therefore a new tool registry
// scoped to that workspace) is required per request. Confuse and Liang have
// no per-request state but are rebuilt alongside Chongzhi for symmetry.
//
// Per-request construction is the documented choice. The alternative —
// passing workspace_root through context — was rejected because (a) the tool
// registry's Execute closures capture workspace_root at registration time and
// cannot be rebound via context, and (b) it would couple the tool layer to
// context plumbing it does not currently understand.
type AgentFactory interface {
	// Build returns a freshly-constructed Confuse agent for the request whose
	// user owns workspaceRoot. The returned Confuse owns its own Chongzhi /
	// Liang sub-agents, all wired to the same LLMClient.
	Build(workspaceRoot string) (Agent, error)
}

// orchestratorFactory is the production AgentFactory. It clones the base
// config and rebuilds the Chongzhi-scoped tool registry per request.
type orchestratorFactory struct {
	cfg          *config.Config
	client       LLMClient
	baseRegistry *tool.Registry // holds non-workspace-scoped tools, if any
}

// Build implements AgentFactory.
func (f *orchestratorFactory) Build(workspaceRoot string) (Agent, error) {
	// Each request gets a fresh registry scoped to the user's workspace.
	// Xizhi tools capture workspaceRoot at registration, so this is mandatory
	// for per-user isolation.
	reqReg := tool.NewRegistry()
	xizhi.RegisterAll(reqReg, workspaceRoot)

	// Carry over any tools registered against the base registry that are not
	// workspace-scoped. Today this is empty (all production tools are Xizhi),
	// but the loop keeps the factory forward-compatible.
	for _, spec := range f.baseRegistry.List() {
		// Skip Xizhi tools — they were re-registered above with the right
		// workspace_root; re-registering would fail (duplicate name).
		if isXizhiTool(spec.Name) {
			continue
		}
		if err := reqReg.Register(spec); err != nil {
			return nil, fmt.Errorf("agent factory: re-register %q: %w", spec.Name, err)
		}
	}

	chongzhi, err := NewChongzhi(f.cfg.Agents.Chongzhi, f.client, reqReg)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build chongzhi: %w", err)
	}
	liang, err := NewLiang(f.cfg.Agents.Liang, f.client)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build liang: %w", err)
	}
	subAgents := map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	}
	confuse, err := NewConfuse(f.cfg.Agents.Confuse, f.client, reqReg, subAgents)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build confuse: %w", err)
	}
	return confuse, nil
}

func isXizhiTool(name string) bool {
	switch name {
	case xizhi.NameReadFile, xizhi.NameWriteFile, xizhi.NameModifyFile:
		return true
	}
	return false
}

// Orchestrator is the top-level entry point that the HTTP handler calls per
// chat request. It builds a per-request agent set via the AgentFactory (so
// Chongzhi binds to the right user workspace), seeds the Confuse loop with
// the user message, runs the loop, and emits a final done event with the
// aggregated token-usage breakdown.
type Orchestrator struct {
	factory AgentFactory
	// workspaceRootForUser is captured for handler convenience; the
	// orchestrator's per-request Build is driven by the workspaceRoot string
	// handed to Handle, not by this function. Phase 9's handler can call this
	// closure to compute workspaceRoot from a user_id before invoking Handle.
	workspaceRootForUser WorkspaceRootForUser
}

// NewOrchestrator constructs the production Orchestrator. workspaceRootForUser
// is an optional convenience closure that maps a user ID to its workspace root
// path; it is stored on the returned Orchestrator so handlers can call
// o.WorkspaceRootForUser(userID) without recomputing the path. It may be nil
// if the handler resolves workspaceRoot some other way. The base registry
// holds any process-wide tools (currently none — all tools are workspace-
// scoped Xizhi tools registered per-request).
func NewOrchestrator(client LLMClient, cfg *config.Config, baseRegistry *tool.Registry, workspaceRootForUser WorkspaceRootForUser) (*Orchestrator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: orchestrator requires non-nil config")
	}
	if client == nil {
		return nil, fmt.Errorf("agent: orchestrator requires non-nil LLM client")
	}
	if baseRegistry == nil {
		baseRegistry = tool.NewRegistry()
	}
	factory := &orchestratorFactory{
		cfg:          cfg,
		client:       client,
		baseRegistry: baseRegistry,
	}
	return &Orchestrator{factory: factory, workspaceRootForUser: workspaceRootForUser}, nil
}

// WorkspaceRootForUser is a function type alias that maps a user ID to the
// absolute path of the user's workspace root. Handlers typically return
// filepath.Join(dataDir, userID, "workspace").
type WorkspaceRootForUser = func(userID string) string

// Handle executes one full chat turn:
//   - Build a per-request agent set via the factory (Chongzhi → user workspace),
//   - Seed Confuse with the user's message,
//   - Run the Confuse loop, streaming events to hub,
//   - Emit a final done event with the aggregated usage breakdown.
//
// workspaceRoot is the absolute path to the requesting user's workspace
// directory (data/{user_uuid}/workspace). The handler computes this and
// passes it in.
func (o *Orchestrator) Handle(ctx context.Context, workspaceRoot, userMessage string, hub *stream.Hub) error {
	confuse, err := o.factory.Build(workspaceRoot)
	if err != nil {
		return fmt.Errorf("orchestrator: build agents: %w", err)
	}

	messages := []Message{{Role: "user", Content: userMessage}}
	content, usage, err := confuse.Run(ctx, messages, hub)
	if err != nil {
		// Even on error we emit done so the SSE client terminates. The
		// Confuse loop already emitted agent_error + agent_end for the
		// failing turn.
		emitDone(hub, ctx, doneUsage{
			confuse: usage,
			content: content,
			err:     err,
		})
		return fmt.Errorf("orchestrator: confuse run: %w", err)
	}

	emitDone(hub, ctx, doneUsage{confuse: usage, content: content})
	return nil
}

// doneUsage is the payload assembled into the final done event's Meta.usage.
type doneUsage struct {
	confuse Usage
	content string
	err     error
}

// emitDone builds the per-agent usage breakdown map and emits the terminal
// done event. Per-agent sub-totals (Chongzhi / Liang) are folded into the
// Confuse usage returned by Run via sub-agent Run aggregation, so the
// top-level Confuse usage already represents the whole turn. We still emit
// the standard per-agent shape so the frontend can render a breakdown table.
func emitDone(hub *stream.Hub, ctx context.Context, u doneUsage) {
	usage := map[string]any{
		"prompt_tokens":     u.confuse.PromptTokens,
		"completion_tokens": u.confuse.CompletionTokens,
		"total_tokens":      u.confuse.TotalTokens,
	}
	if u.err != nil {
		usage["error"] = u.err.Error()
		logger.L().Warn("orchestrator completed with error",
			zap.Error(u.err),
			zap.Int("total_tokens", u.confuse.TotalTokens))
	}
	if !hub.SendCtx(ctx, stream.DoneEvent(usage)) {
		return
	}
}
