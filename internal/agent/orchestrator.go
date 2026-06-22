package agent

import (
	"context"
	"fmt"
	"runtime"
	"slices"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/prompt"
	"github.com/lush/blowball/internal/stream"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/luban"
	"github.com/lush/blowball/internal/tool/skill"
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
	Build(workspaceRoot, skillsDir, userID string) (Agent, error)
}

// orchestratorFactory is the production AgentFactory. It clones the base
// config and rebuilds the per-agent tool registry and system prompt per request.
type orchestratorFactory struct {
	cfg          *config.Config
	client       LLMClient
	baseRegistry *tool.Registry      // holds process-wide tools (webfetch, MCP proxies, luban skill tools, optional read_skill)
	serverTools  map[string][]string // server name -> prefixed tool names
	skillLoader  *skill.Loader       // discovers global and per-user skills
}

// Build implements AgentFactory.
func (f *orchestratorFactory) Build(workspaceRoot, skillsDir, userID string) (Agent, error) {
	// agent.skills must reference global skills only; user skills are discovered
	// at runtime via luban tools.
	if err := f.cfg.ValidateAgentSkills(userID, func(name, _ string) bool {
		return f.skillLoader.HasSkill(name, "")
	}); err != nil {
		return nil, fmt.Errorf("agent factory: validate skills: %w", err)
	}

	chongzhi, err := f.buildChongzhi(workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build chongzhi: %w", err)
	}
	liang, err := f.buildLiang(workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build liang: %w", err)
	}
	subAgents := map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	}
	confuse, err := f.buildConfuse(workspaceRoot, skillsDir, userID, subAgents)
	if err != nil {
		return nil, fmt.Errorf("agent factory: build confuse: %w", err)
	}
	return confuse, nil
}

func (f *orchestratorFactory) buildConfuse(workspaceRoot, skillsDir, userID string, subAgents map[string]Agent) (*Confuse, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Confuse, workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, err
	}
	return NewConfuse(cfg, f.client, reg, subAgents)
}

func (f *orchestratorFactory) buildChongzhi(workspaceRoot, skillsDir, userID string) (*Chongzhi, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Chongzhi, workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, err
	}
	return NewChongzhi(cfg, f.client, reg)
}

func (f *orchestratorFactory) buildLiang(workspaceRoot, skillsDir, userID string) (*Liang, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Liang, workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, err
	}
	return NewLiang(cfg, f.client, reg)
}

// buildAgentRegistry creates a registry scoped to workspaceRoot containing the
// tools this agent is allowed to use: built-ins listed in cfg.Tools, MCP tools
// allowed by cfg.MCP, and luban skill tools when they are configured.
func (f *orchestratorFactory) buildAgentRegistry(cfg config.AgentConfig, workspaceRoot, skillsDir, userID string) (*tool.Registry, config.AgentConfig, error) {
	mcpToolNames := f.allowedMCPTools(cfg.MCP)
	fullToolNames := append([]string(nil), cfg.Tools...)
	fullToolNames = append(fullToolNames, mcpToolNames...)

	allowed := make(map[string]struct{}, len(fullToolNames))
	for _, n := range fullToolNames {
		allowed[n] = struct{}{}
	}

	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, workspaceRoot, f.cfg.Tools.Xizhi)

	for _, spec := range f.baseRegistry.List() {
		// Skip Xizhi tools — they were re-registered above with the right
		// workspace_root; re-registering would fail (duplicate name).
		if isXizhiTool(spec.Name) {
			continue
		}
		if _, ok := allowed[spec.Name]; !ok {
			continue
		}
		if err := reg.Register(spec); err != nil {
			return nil, cfg, fmt.Errorf("agent factory: re-register %q: %w", spec.Name, err)
		}
	}

	rendered, err := f.renderSystemPrompt(cfg, workspaceRoot, skillsDir, userID)
	if err != nil {
		return nil, cfg, err
	}
	cfg.Tools = fullToolNames
	cfg.SystemPrompt = rendered
	return reg, cfg, nil
}

// allowedMCPTools returns the prefixed tool names this agent is allowed to use
// based on its MCP configuration. A server entry with tools ["*"] allows every
// tool discovered from that server.
func (f *orchestratorFactory) allowedMCPTools(mcp config.AgentMCPConfig) []string {
	var out []string
	for _, s := range mcp.Servers {
		known := f.serverTools[s.Name]
		if len(s.Tools) == 1 && s.Tools[0] == "*" {
			out = append(out, known...)
			continue
		}
		out = append(out, s.Tools...)
	}
	return out
}

func isXizhiTool(name string) bool {
	switch name {
	case xizhi.NameReadFile, xizhi.NameWriteFile, xizhi.NameModifyFile,
		xizhi.NameListFiles, xizhi.NameTree, xizhi.NameGlobFiles:
		return true
	}
	return false
}

func isLubanTool(name string) bool {
	switch name {
	case luban.ToolListSkills, luban.ToolReadSkill, luban.ToolInstallSkill:
		return true
	}
	return false
}

// renderSystemPrompt builds the complete system prompt for an agent.
func (f *orchestratorFactory) renderSystemPrompt(cfg config.AgentConfig, workspaceRoot, skillsDir, userID string) (string, error) {
	return prompt.RenderSystemPrompt(prompt.RenderInput{
		BasePrompt: cfg.SystemPrompt,
		Workspace:  workspaceRoot,
		SkillsDir:  skillsDir,
		UserID:     userID,
		Platform:   runtime.GOARCH,
		OS:         runtime.GOOS,
		Cutoff:     "August 2025",
		Tools:      f.collectTools(cfg),
		Skills:     f.collectSkills(cfg),
	})
}

// collectTools converts the tools allowed for this agent into prompt.ToolInfo
// values, preserving the built-in / MCP-server grouping.
func (f *orchestratorFactory) collectTools(cfg config.AgentConfig) []prompt.ToolInfo {
	builtIn, mcpByServer := f.classifyTools(cfg)
	var tools []prompt.ToolInfo
	for _, spec := range builtIn {
		tools = append(tools, prompt.ToolInfo{Name: spec.Name, Description: spec.Description})
	}
	for serverName, specs := range mcpByServer {
		for _, spec := range specs {
			tools = append(tools, prompt.ToolInfo{Name: spec.Name, Description: spec.Description, Server: serverName})
		}
	}
	return tools
}

// collectSkills converts the global skills allowed for this agent into
// prompt.SkillInfo values. Only global skills are injected into the system
// prompt; user skills are discovered at runtime via luban_list_skills.
func (f *orchestratorFactory) collectSkills(cfg config.AgentConfig) []prompt.SkillInfo {
	if len(cfg.Skills) == 0 {
		return nil
	}
	allSkills := f.skillLoader.ListGlobal()
	allowed := skill.Filter(allSkills, cfg.Skills)
	var skills []prompt.SkillInfo
	for _, s := range allowed {
		skills = append(skills, prompt.SkillInfo{Name: s.Name, Description: s.Description, Location: s.Location})
	}
	return skills
}

// classifyTools splits the tools relevant to this agent into built-in tools and
// MCP tools grouped by server. The classification uses the serverTools mapping
// populated at MCP registration time.
func (f *orchestratorFactory) classifyTools(cfg config.AgentConfig) ([]*tool.ToolSpec, map[string][]*tool.ToolSpec) {
	allowedMCP := make(map[string]struct{})
	for _, name := range f.allowedMCPTools(cfg.MCP) {
		allowedMCP[name] = struct{}{}
	}
	toolToServer := make(map[string]string)
	for serverName, names := range f.serverTools {
		for _, n := range names {
			toolToServer[n] = serverName
		}
	}

	var builtIn []*tool.ToolSpec
	mcpByServer := make(map[string][]*tool.ToolSpec)
	for _, spec := range f.baseRegistry.List() {
		if isXizhiTool(spec.Name) {
			continue
		}
		if _, ok := allowedMCP[spec.Name]; ok {
			serverName := toolToServer[spec.Name]
			mcpByServer[serverName] = append(mcpByServer[serverName], spec)
			continue
		}
		// Treat luban skill tools as built-in so they appear in the system prompt
		// regardless of whether the agent also has them in cfg.Tools.
		if isLubanTool(spec.Name) {
			builtIn = append(builtIn, spec)
			continue
		}
		// Only include built-ins that are explicitly listed in cfg.Tools so we
		// do not leak unrelated process-wide tools.
		if slices.Contains(cfg.Tools, spec.Name) {
			builtIn = append(builtIn, spec)
		}
	}
	return builtIn, mcpByServer
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
// o.WorkspaceRootForUser(userID) without recomputing the path. serverTools is
// the server-name -> tool-names mapping produced by mcpclient registration;
// skillLoader discovers global and per-user skills for system prompt injection.
func NewOrchestrator(client LLMClient, cfg *config.Config, baseRegistry *tool.Registry, serverTools map[string][]string, skillLoader *skill.Loader, workspaceRootForUser WorkspaceRootForUser) (*Orchestrator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent: orchestrator requires non-nil config")
	}
	if client == nil {
		return nil, fmt.Errorf("agent: orchestrator requires non-nil LLM client")
	}
	if baseRegistry == nil {
		baseRegistry = tool.NewRegistry()
	}
	if serverTools == nil {
		serverTools = make(map[string][]string)
	}
	factory := &orchestratorFactory{
		cfg:          cfg,
		client:       client,
		baseRegistry: baseRegistry,
		serverTools:  serverTools,
		skillLoader:  skillLoader,
	}
	return &Orchestrator{factory: factory, workspaceRootForUser: workspaceRootForUser}, nil
}

// WorkspaceRootForUser is a function type alias that maps a user ID to the
// absolute path of the user's workspace root. Handlers typically return
// filepath.Join(dataDir, userID, "workspace").
type WorkspaceRootForUser = func(userID string) string

// Handle executes one full chat turn:
//   - Build a per-request agent set via the factory (Chongzhi → user workspace),
//   - Run the Confuse loop with the provided conversation history,
//   - Stream events to hub,
//   - Emit a final done event with the aggregated usage breakdown.
//
// workspaceRoot is the absolute path to the requesting user's workspace
// directory (data/{user_uuid}/workspace). userID identifies the caller so the
// factory can load user-specific skills and validate skill permissions.
func (o *Orchestrator) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []Message, hub *stream.Hub) error {
	ctx = skill.WithUserID(ctx, userID)
	confuse, err := o.factory.Build(workspaceRoot, skillsDir, userID)
	if err != nil {
		return fmt.Errorf("orchestrator: build agents: %w", err)
	}

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
	if u.confuse.ReasoningTokens > 0 {
		usage["reasoning_tokens"] = u.confuse.ReasoningTokens
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
