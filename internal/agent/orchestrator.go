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
	"github.com/lush/blowball/internal/tool/mcp"
	"github.com/lush/blowball/internal/tool/skill"
	"github.com/lush/blowball/internal/tool/xizhi"
	"go.uber.org/zap"
)

// AgentFactory builds a fresh set of agents for a single request. The Chongzhi
// agent binds Xizhi tools against the requesting user's workspace_root, which
// varies per request, so a new Chongzhi (and therefore a new tool registry
// scoped to that workspace) is required per request. Confucius and Liang have
// no per-request state but are rebuilt alongside Chongzhi for symmetry.
//
// Per-request construction is the documented choice. The alternative —
// passing workspace_root through context — was rejected because (a) the tool
// registry's Execute closures capture workspace_root at registration time and
// cannot be rebound via context, and (b) it would couple the tool layer to
// context plumbing it does not currently understand.
type AgentFactory interface {
	// Build returns a freshly-constructed Confucius agent for the request whose
	// user owns workspaceRoot. The returned Confucius owns its own Chongzhi /
	// Liang sub-agents, all wired to the same LLMClient. The returned TurnCloser
	// releases per-turn resources (the per-user MCP connection manager) once the
	// turn's Run completes; it is nil when no per-turn resources were created.
	Build(workspaceRoot, skillsDir, userID string) (Agent, TurnCloser, error)
}

// TurnCloser releases per-turn resources created by AgentFactory.Build (the
// turn-scoped, per-user MCP connection manager). The orchestrator invokes it
// after the turn's Run completes. A nil TurnCloser means nothing to clean up.
type TurnCloser func()

// orchestratorFactory is the production AgentFactory. It clones the base
// config and rebuilds the per-agent tool registry and system prompt per request.
type orchestratorFactory struct {
	cfg          *config.Config
	client       LLMClient
	baseRegistry *tool.Registry      // holds process-wide tools (webfetch, MCP proxies, luban skill tools)
	serverTools  map[string][]string // server name -> prefixed tool names
	skillLoader  *skill.Loader       // discovers global and per-user skills
	userMCP      config.UserMCPConfig // per-user MCP tool timeouts/enabled flag
}

// Build implements AgentFactory.
func (f *orchestratorFactory) Build(workspaceRoot, skillsDir, userID string) (Agent, TurnCloser, error) {
	// agent.skills must reference global skills only; user skills are discovered
	// at runtime via luban tools.
	if err := f.cfg.ValidateAgentSkills(userID, func(name, _ string) bool {
		return f.skillLoader.HasSkill(name, "")
	}); err != nil {
		return nil, nil, fmt.Errorf("agent factory: validate skills: %w", err)
	}

	globalSkillsDir := f.skillLoader.GlobalDir()

	// Per-user MCP connection manager is created once per turn and shared
	// across Confucius + its sub-agents so connections are reused within the
	// turn and destroyed at turn end. It is only created when at least one agent
	// lists an mcp_* tool, to avoid needless allocation otherwise. The manager
	// is bound to this user's workspaceRoot, so credential isolation holds by
	// construction (see design D1/D5).
	var mcpMgr *mcp.Manager
	var closer TurnCloser
	if mcp.AnyMCPTool(f.cfg.Agents.Confucius.Tools, f.cfg.Agents.Chongzhi.Tools, f.cfg.Agents.Liang.Tools) {
		mcpMgr = mcp.NewManager(mcp.ManagerOptions{
			WorkspaceRoot:  workspaceRoot,
			ConnectTimeout: f.userMCP.ConnectTimeout,
			CallTimeout:    f.userMCP.CallTimeout,
		})
		closer = func() { _ = mcpMgr.Close() }
	}

	chongzhi, err := f.buildChongzhi(workspaceRoot, globalSkillsDir, skillsDir, userID, mcpMgr)
	if err != nil {
		if closer != nil {
			closer()
		}
		return nil, nil, fmt.Errorf("agent factory: build chongzhi: %w", err)
	}
	liang, err := f.buildLiang(workspaceRoot, globalSkillsDir, skillsDir, userID, mcpMgr)
	if err != nil {
		if closer != nil {
			closer()
		}
		return nil, nil, fmt.Errorf("agent factory: build liang: %w", err)
	}
	subAgents := map[string]Agent{
		ToolInvokeChongzhi: chongzhi,
		ToolInvokeLiang:    liang,
	}
	confucius, err := f.buildConfucius(workspaceRoot, globalSkillsDir, skillsDir, userID, subAgents, mcpMgr)
	if err != nil {
		if closer != nil {
			closer()
		}
		return nil, nil, fmt.Errorf("agent factory: build confucius: %w", err)
	}
	return confucius, closer, nil
}

func (f *orchestratorFactory) buildConfucius(workspaceRoot, globalSkillsDir, userSkillsDir, userID string, subAgents map[string]Agent, mcpMgr *mcp.Manager) (*Confucius, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Confucius, workspaceRoot, globalSkillsDir, userSkillsDir, userID, mcpMgr)
	if err != nil {
		return nil, err
	}
	return NewConfucius(cfg, f.client, reg, subAgents)
}

func (f *orchestratorFactory) buildChongzhi(workspaceRoot, globalSkillsDir, userSkillsDir, userID string, mcpMgr *mcp.Manager) (*Chongzhi, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Chongzhi, workspaceRoot, globalSkillsDir, userSkillsDir, userID, mcpMgr)
	if err != nil {
		return nil, err
	}
	return NewChongzhi(cfg, f.client, reg)
}

func (f *orchestratorFactory) buildLiang(workspaceRoot, globalSkillsDir, userSkillsDir, userID string, mcpMgr *mcp.Manager) (*Liang, error) {
	reg, cfg, err := f.buildAgentRegistry(f.cfg.Agents.Liang, workspaceRoot, globalSkillsDir, userSkillsDir, userID, mcpMgr)
	if err != nil {
		return nil, err
	}
	return NewLiang(cfg, f.client, reg)
}

// buildAgentRegistry creates a registry scoped to workspaceRoot containing the
// tools this agent is allowed to use: built-ins listed in cfg.Tools, MCP tools
// allowed by cfg.MCP, luban skill tools when they are configured, and the
// per-user mcp_* tools when the agent lists one and a turn-scoped manager is
// available.
func (f *orchestratorFactory) buildAgentRegistry(cfg config.AgentConfig, workspaceRoot, globalSkillsDir, userSkillsDir, userID string, mcpMgr *mcp.Manager) (*tool.Registry, config.AgentConfig, error) {
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

	// Per-user mcp_* tools. They are registered per-turn (bound to the
	// turn-scoped, per-user manager) rather than copied from the base registry,
	// because they hold per-turn connection state. Only registered when the
	// agent lists one and a manager exists (created once per turn in Build).
	if mcpMgr != nil && agentListsMCPTool(cfg.Tools) {
		if err := mcp.RegisterAll(reg, mcp.NewTools(mcpMgr)); err != nil {
			return nil, cfg, fmt.Errorf("agent factory: register mcp tools: %w", err)
		}
	}

	rendered, err := f.renderSystemPrompt(cfg, workspaceRoot, globalSkillsDir, userSkillsDir, userID, reg, mcpMgr)
	if err != nil {
		return nil, cfg, err
	}
	cfg.Tools = fullToolNames
	cfg.SystemPrompt = rendered
	return reg, cfg, nil
}

// agentListsMCPTool reports whether the agent's configured tools include any
// per-user mcp_* tool.
func agentListsMCPTool(tools []string) bool {
	for _, name := range tools {
		if mcp.IsMCPTool(name) {
			return true
		}
	}
	return false
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
		xizhi.NameListFiles, xizhi.NameTree, xizhi.NameGlobFiles, xizhi.NameGrep, xizhi.NameDeleteFile:
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
func (f *orchestratorFactory) renderSystemPrompt(cfg config.AgentConfig, workspaceRoot, globalSkillsDir, userSkillsDir, userID string, reg *tool.Registry, mcpMgr *mcp.Manager) (string, error) {
	tools := f.collectTools(cfg)
	// Per-user mcp_* tools are registered per-turn (not in the base registry),
	// so surface them from the per-agent registry so the prompt advertises the
	// same tools the model can actually call.
	if reg != nil {
		for _, spec := range reg.List() {
			if mcp.IsMCPTool(spec.Name) {
				tools = append(tools, prompt.ToolInfo{Name: spec.Name, Description: spec.Description})
			}
		}
	}

	input := prompt.RenderInput{
		BasePrompt:      cfg.SystemPrompt,
		Workspace:       workspaceRoot,
		GlobalSkillsDir: "/skills/global",
		UserSkillsDir:   "/workspace/.blowball/skills",
		UserID:          userID,
		Platform:        runtime.GOARCH,
		OS:              runtime.GOOS,
		Cutoff:          "August 2025",
		Tools:           tools,
		Skills:          f.collectSkills(cfg),
	}
	// Advertise the caller's per-user MCP servers (server-level name +
	// description only) when the family is active. Credentials are never loaded
	// into the prompt — only name/url/description are rendered.
	if mcpMgr != nil {
		input.UserMCP = collectUserMCPServers(workspaceRoot)
	}
	return prompt.RenderSystemPrompt(input)
}

// collectUserMCPServers reads the caller's per-user MCP config and returns the
// server-level descriptions for system-prompt advertisement. A missing or
// unreadable config yields no servers (the read error is logged but never
// crashes the turn — see the "malformed config does not crash" requirement).
func collectUserMCPServers(workspaceRoot string) []prompt.MCPServerInfo {
	cfg, err := mcp.LoadConfig(workspaceRoot)
	if err != nil {
		logger.L().Warn("load per-user mcp config for prompt failed; skipping user mcp advertisement",
			zap.String("workspace", workspaceRoot),
			zap.Error(err))
		return nil
	}
	out := make([]prompt.MCPServerInfo, 0, len(cfg.Servers))
	for _, s := range cfg.SortedServers() {
		out = append(out, prompt.MCPServerInfo{Name: s.Name, Description: s.Description, URL: s.URL})
	}
	return out
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
// Chongzhi binds to the right user workspace), seeds the Confucius loop with
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
	if skillLoader == nil {
		skillLoader = skill.NewLoader("", nil)
	}
	factory := &orchestratorFactory{
		cfg:          cfg,
		client:       client,
		baseRegistry: baseRegistry,
		serverTools:  serverTools,
		skillLoader:  skillLoader,
		userMCP:      cfg.Tools.UserMCP,
	}
	return &Orchestrator{factory: factory, workspaceRootForUser: workspaceRootForUser}, nil
}

// WorkspaceRootForUser is a function type alias that maps a user ID to the
// absolute path of the user's workspace root. Handlers typically return
// filepath.Join(dataDir, userID, "workspace").
type WorkspaceRootForUser = func(userID string) string

// Handle executes one full chat turn:
//   - Build a per-request agent set via the factory (Chongzhi → user workspace),
//   - Run the Confucius loop with the provided conversation history,
//   - Stream events to hub,
//   - Emit a final done event with the aggregated usage breakdown.
//
// workspaceRoot is the absolute path to the requesting user's workspace
// directory (data/{user_uuid}/workspace). userID identifies the caller so the
// factory can load user-specific skills and validate skill permissions.
func (o *Orchestrator) Handle(ctx context.Context, workspaceRoot, skillsDir, userID string, messages []Message, hub *stream.Hub) error {
	ctx = skill.WithUserID(ctx, userID)
	confucius, closer, err := o.factory.Build(workspaceRoot, skillsDir, userID)
	if err != nil {
		return fmt.Errorf("orchestrator: build agents: %w", err)
	}
	// Release per-turn resources (the per-user MCP connection manager) once the
	// turn's Run completes, regardless of success/failure. The closer is nil
	// when no per-turn resources were created.
	if closer != nil {
		defer closer()
	}

	content, usage, breakdown, err := confucius.Run(ctx, messages, hub)
	if err != nil {
		// Even on error we emit done so the SSE client terminates. The
		// Confucius loop already emitted agent_error + agent_end for the
		// failing turn.
		emitDone(hub, ctx, doneUsage{
			confucius: usage,
			breakdown: breakdown,
			content:   content,
			err:       err,
		})
		return fmt.Errorf("orchestrator: confucius run: %w", err)
	}

	emitDone(hub, ctx, doneUsage{confucius: usage, breakdown: breakdown, content: content})
	return nil
}

// doneUsage is the payload assembled into the final done event's Meta.usage.
// confucius is the aggregate turn usage (Confucius + all dispatched
// sub-agents); breakdown is the per-agent attribution + orchestration meta
// Confucius assembled during its dispatch loop (Confucius's own usage under
// "Confucius", each dispatched sub-agent under its display name, plus the
// parallel flag and ordered invoke_* list). Both are persisted into
// turn_usage so historical cost is queryable per agent and per session.
type doneUsage struct {
	confucius Usage
	breakdown *TurnBreakdown
	content   string
	err       error
}

// emitDone builds the authoritative per-agent usage object and emits the
// terminal done event. The usage object shape is (see the turn-cost-tracking
// spec, authoritative shape):
//
//	{
//	  "total":     {prompt_tokens, completion_tokens, total_tokens, reasoning_tokens?},
//	  "by_agent":  {<agent display name>: {prompt_tokens, ...}},
//	  "meta":      {sub_agent_invocations: [...], parallel: bool},
//	}
//
// `total` is the aggregate turn usage. `by_agent` carries the per-agent
// breakdown Confucius assembled; it always includes "Confucius" and one entry
// per dispatched sub-agent. `meta` records whether any assistant round
// dispatched >=2 tool_calls (parallel) and which invoke_* sub-agents fired
// (sub_agent_invocations, deduplicated, dispatch order). On error, an `error`
// field is added at the top level describing the failure.
func emitDone(hub *stream.Hub, ctx context.Context, u doneUsage) {
	usage := map[string]any{
		"total":    usageObject(u.confucius),
		"by_agent": buildByAgentObject(u.breakdown),
		"meta":     buildMetaObject(u.breakdown),
	}
	if u.err != nil {
		usage["error"] = u.err.Error()
		logger.L().Warn("orchestrator completed with error",
			zap.Error(u.err),
			zap.Int("total_tokens", u.confucius.TotalTokens))
	}
	if !hub.SendCtx(ctx, stream.DoneEvent(usage)) {
		return
	}
}

// buildByAgentObject renders the breakdown's per-agent map into the done
// event's usage.by_agent object: {<agent name>: usageObject(...)}. When the
// breakdown is nil (no dispatch happened, e.g. an error before Run produced a
// breakdown) it returns an empty object.
func buildByAgentObject(b *TurnBreakdown) map[string]any {
	if b == nil || len(b.ByAgent) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(b.ByAgent))
	for name, u := range b.ByAgent {
		out[name] = usageObject(u)
	}
	return out
}

// buildMetaObject renders the breakdown's orchestration meta into the done
// event's usage.meta object: {sub_agent_invocations: [...], parallel: bool}.
// When the breakdown is nil it returns an empty meta with a false parallel flag
// and an empty invocations list.
func buildMetaObject(b *TurnBreakdown) map[string]any {
	m := map[string]any{
		"sub_agent_invocations": []string{},
		"parallel":              false,
	}
	if b == nil {
		return m
	}
	m["parallel"] = b.Parallel
	if len(b.SubAgentInvocations) > 0 {
		m["sub_agent_invocations"] = append([]string(nil), b.SubAgentInvocations...)
	}
	return m
}
