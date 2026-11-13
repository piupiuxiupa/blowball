// Package agentserver assembles the agent-role partition: the tool registry,
// the external MCP manager, the OpenAI client (with raw-capture sink), the
// orchestrator, the title/compaction services, cross-session memory, the
// streaming + MCP-tool handlers, the scheduled-execution runtime, and the
// session message queue.
//
// It is consumed by the development monolith (cmd/blowball, --role all|agent)
// and the blowball-agent service binary. The api assembly never depends on
// anything built here.
package agentserver

import (
	"context"
	"runtime"
	"slices"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	"github.com/lush/blowball/pkg/agent"
	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/handler"
	"github.com/lush/blowball/pkg/llmraw"
	"github.com/lush/blowball/pkg/memory"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/scheduledcontrol"
	"github.com/lush/blowball/pkg/scheduledruntime"
	"github.com/lush/blowball/pkg/service"
	"github.com/lush/blowball/pkg/tool"
	"github.com/lush/blowball/pkg/tool/executor"
	"github.com/lush/blowball/pkg/tool/luban"
	"github.com/lush/blowball/pkg/tool/mcpclient"
	"github.com/lush/blowball/pkg/tool/scheduledtask"
	"github.com/lush/blowball/pkg/tool/skill"
	"github.com/lush/blowball/pkg/tool/webfetch"
	"github.com/lush/blowball/pkg/tool/xizhi"
	"github.com/lush/blowball/pkg/turn"
)

// Assembly is the agent partition's wiring result: the agent RouteDeps plus
// every long-lived object whose lifecycle the serve loop must coordinate.
// Shutdown/Close keep the drain ordering in one place so the monolith and the
// blowball-agent binary cannot drift apart.
type Assembly struct {
	Routes handler.RouteDeps
	// MCPManager holds the operator MCP connections; closed via Close.
	MCPManager *mcpclient.Manager
	// RawFlusher drains the raw-capture write-behind buffer; nil never (the
	// agent role always captures).
	RawFlusher *llmraw.Flusher
	// RunRegistry holds every running turn's cancel for bounded shutdown.
	RunRegistry *run.Registry
	// Memory is the cross-session memory client, nil when disabled.
	Memory *memory.Service

	// stopAdmission gates new turns (interactive queue + scheduled runtime)
	// before anything drains. Always non-nil.
	stopAdmission func()
}

// StopAdmission gates new interactive/background turns FIRST so no new turn
// can cross a startup fence while shutdown drains existing work. Queued
// entries survive in Redis for the post-restart reconciler.
func (a *Assembly) StopAdmission() {
	a.stopAdmission()
}

// Finalize runs the ordered drain AFTER the HTTP listener and internal RPC
// server have stopped: cancel every running turn (each cancelled turn
// persists its partial output through the ordinary interrupted-turn path),
// wait for persistence completion, then drain the raw-capture buffer and
// release the memory client's idle keep-alive connections. In-flight memory
// captures are not aborted: whatever landed on the OpenViking session stays
// pending and the session's next turn commits it — at-least-once.
func (a *Assembly) Finalize(log *zap.Logger) {
	if a.RunRegistry != nil {
		if n := a.RunRegistry.CancelAll(); n > 0 {
			log.Info("cancelling running turns for shutdown", zap.Int("turns", n))
			wctx, wcancel := context.WithTimeout(context.Background(), boot.ShutdownTimeout)
			if err := a.RunRegistry.WaitAll(wctx); err != nil {
				log.Warn("shutdown turn wait timed out; proceeding", zap.Error(err))
			}
			wcancel()
		}
	}
	if a.RawFlusher != nil {
		a.RawFlusher.Close()
		log.Info("llm raw flusher stopped")
	}
	if a.Memory != nil {
		a.Memory.Close()
	}
}

// Close releases the MCP manager's connections. Defer it right after Wire so
// it runs last, after Finalize has drained the capture buffer.
func (a *Assembly) Close(log *zap.Logger) {
	if a.MCPManager != nil {
		if cerr := a.MCPManager.Close(); cerr != nil {
			log.Warn("mcp client close failed", zap.Error(cerr))
		}
	}
}

// Wire builds the agent layer for the agent role (and contributes it in the
// all role): the tool registry, the external MCP manager, the OpenAI client
// (with raw-capture sink attached), the orchestrator, the title service, and
// the streaming + MCP-tool handlers. internalSrv is the internal gRPC server
// the scheduled-execution runtime registers on (nil when internal_rpc is
// off — the block is then inert).
func Wire(rt *boot.Runtime, sessSvc *service.SessionService, internalSrv *grpc.Server) *Assembly {
	cfg := rt.Cfg
	dataDir := rt.DataDir
	fsStore := rt.FSStore
	log := rt.Log

	// Tool registry. The main registry backs the MCP tools-listing endpoint. Real tool execution during orchestration uses a per-request registry the orchestrator's factory rebuilds scoped to the user's workspace root.
	reg := tool.NewRegistry()
	reg.SetTimeouts(cfg.Tools.Timeouts)

	// Sandboxed bash execution. Only registered on Linux where bwrap is available; on other platforms the enabled tool is ignored. If bash is explicitly enabled but bwrap is missing on Linux, startup fails fast. (The dedicated python/pip_install executors were removed; Python code and pip installs run via bash.)
	if boot.ExecutorConfigured(cfg) {
		if !executor.IsAvailable() {
			log.Fatal("executor tools enabled but bubblewrap (bwrap) is not available",
				zap.String("platform", runtime.GOOS))
		}
		// cfg.Tools.Executor carries the parsed bwrap sandbox policy (Sandbox:
		// stat-guarded system baseline + extra RO/RW mounts), so it threads
		// straight into NewTools → buildBwrapArgs. Per-user skills live under
		// the workspace at .blowball/skills and reach the sandbox via the
		// /workspace bind, so only the workspace resolver is needed here.
		executorTools := executor.NewTools(cfg.Tools.Executor, func(userID string) string {
			return fsStore.UserWorkspace(userID)
		}, rt.SkillsDir, rt.ToolsDir).WithMarket(rt.Market)
		if err := executor.RegisterAll(reg, executorTools); err != nil {
			log.Fatal("register executor tools failed", zap.Error(err))
		}
	}

	// Skill loader. Discover skills from the global skills directory and per-user data/{userID}/skills/ directories. Register the luban skill tools globally when at least one agent lists them.
	skillLoader := skill.NewLoader(rt.SkillsDir, func(userID string) string {
		return fsStore.UserSkills(userID)
	})
	if needsLubanTools(cfg.Agents) {
		lubanTools := luban.NewTools(skillLoader, func(userID string) string {
			return fsStore.UserSkills(userID)
		}).WithMarket(rt.Market)
		if err := luban.RegisterAll(reg, lubanTools); err != nil {
			log.Fatal("register luban tools failed", zap.Error(err))
		}
	}

	// Per-user mcp_* tools (mcp_list_servers / mcp_add_server / mcp_remove_server /
	// mcp_call) are NOT registered into this process-wide registry. Unlike luban,
	// they hold per-turn, per-user connection state (the turn-scoped MCP
	// connection manager), so the orchestrator's per-request AgentFactory builds
	// and binds them fresh for each turn against the requesting user's workspace.
	// Only the agent/all role builds the orchestrator, so the api role never
	// surfaces these tools (its SessionHandler is CRUD-only). The family activates
	// automatically when any agent lists an mcp_* tool in config.

	// External MCP servers. Connect, discover tools, and register proxy specs into the process-wide registry. Startup fails fast on connection or tool-list errors.
	mcpManager, err := mcpclient.RegisterAllWithManager(context.Background(), reg, cfg.MCP)
	if err != nil {
		log.Fatal("mcp client registration failed", zap.Error(err))
	}

	// Validate agent MCP tool references against the discovered remote tools.
	serverTools := mcpManager.ServerTools()
	if err := cfg.ValidateAgentMCPTools(toServerToolSet(serverTools)); err != nil {
		log.Fatal("agent mcp tool validation failed", zap.Error(err))
	}

	// Validate agent skill references against global skills. Per-user skills are validated at request time when the userID is known.
	if err := cfg.ValidateAgentSkills("", skillLoader.HasSkill); err != nil {
		log.Fatal("agent skill validation failed", zap.Error(err))
	}

	// Message service delegates saves back to SessionService.SaveMessage so writes stay in one place.
	msgSvc := service.NewMessageService(service.SessionDeps{MySQL: rt.MySQLStore, Redis: rt.RedisStore, FS: fsStore}, sessSvc.SaveMessage)

	// Raw LLM capture (llm-raw-capture capability): the sink stages captured
	// request/response/error payloads in the Redis write-behind buffer and the
	// flusher batch-inserts them into llm_raw_log. Zero-config, always on for
	// agent/all roles; Finalize closes the flusher after the HTTP shutdown so
	// the buffer drains before the stores are released.
	rawNotify := make(chan struct{}, 1)
	rawSink := llmraw.NewSink(rt.RedisStore, rawNotify)
	rawFlusher := llmraw.NewFlusher(rt.RedisStore, rt.MySQLStore, rawNotify)
	rawFlusher.Start()

	openAIClient := agent.NewOpenAIClientWithSink(cfg.OpenAI, rawSink)
	// Per-user LLM credentials (user-llm-token): every LLM call attributed to
	// a user — the turn's whole spawn tree, title generation, compaction
	// summaries and webfetch digestion — flows through this resolver. A user
	// with a stored token hits the shared openai.base_url with their own
	// credential; everyone else (and any call without a user identity) uses
	// the global openai.api_key client unchanged. Credential resolution
	// failures fail the call explicitly — never a silent global-key retry.
	llmClient := agent.NewClientResolver(openAIClient, cfg.OpenAI, rawSink, rt.MySQLStore)
	var webfetchDigester webfetch.ContentDigester
	if cfg.Tools.Webfetch.Digest.Enabled {
		promptClient, err := agent.NewWebfetchPromptClient(llmClient, cfg.OpenAI, cfg.Tools.Webfetch.Digest.Model)
		if err != nil {
			log.Fatal("webfetch digest model resolution failed", zap.Error(err))
		}
		webfetchDigester = webfetch.NewDigester(
			promptClient,
			cfg.Tools.Webfetch.Digest,
			cfg.DefaultModelName(),
			func(userID string) string { return fsStore.UserWorkspace(userID) },
		)
	}
	webfetch.RegisterAllWithDigester(reg, cfg.Tools.Webfetch, webfetchDigester)

	// Workspace image understanding sidecar (xizhi-view-image). Registered
	// into the base registry for the MCP tools-list endpoint; the real
	// execution path re-registers it per request against the user's workspace
	// inside the orchestrator factory. The vision model was validated against
	// the catalog at config load, so resolution here cannot fail.
	var visionClient xizhi.VisionClient
	if cfg.Tools.Xizhi.ViewImage.Enabled {
		vc, err := agent.NewVisionClient(llmClient, cfg.OpenAI, cfg.Tools.Xizhi.ViewImage.Model, cfg.Tools.Xizhi.ViewImage.Resolve().MaxOutputTokens)
		if err != nil {
			log.Fatal("xizhi view_image model resolution failed", zap.Error(err))
		}
		visionClient = vc
	}
	xizhi.RegisterAll(reg, dataDir, cfg.Tools.Xizhi, visionClient, rt.ResourceGuard)

	// Title generation runs on its own resolved model (openai.title_model or
	// the default catalog entry) over the shared resolver-wrapped client, so
	// the title call bills to the triggering user's token.
	titleCfg := cfg.OpenAI
	titleCfg.TitleModel = cfg.TitleModelName()
	titleSvc := service.NewTitleService(llmClient, rt.MySQLStore, titleCfg)

	// Context-compaction service (context-compaction capability): reuses the
	// shared OpenAI client (summary calls land in llm_raw_log). The summary
	// fallback is the deployment default model; the max-context argument is
	// the FALLBACK limit — the default catalog entry's window (the catalog is
	// mandatory, so compaction is always armed). The handler substitutes the
	// request-resolved model's window per turn.
	defaultMaxContext := 0
	if e, ok := cfg.OpenAI.FindModelCatalogEntry(cfg.DefaultModelName()); ok {
		defaultMaxContext = e.MaxContextTokens
	}
	compSvc := service.NewCompactionService(
		service.SessionDeps{MySQL: rt.MySQLStore, Redis: rt.RedisStore, FS: fsStore},
		llmClient,
		cfg.DefaultModelName(),
		defaultMaxContext,
	)

	// Cross-session memory (cross-session-memory capability): default OFF —
	// NewService returns nil for a disabled config and the streaming handler's
	// nil-safe Enabled() gate makes that byte-for-byte pre-capability
	// behavior. The api role never constructs this. The startup health probe
	// is best-effort by contract (memory degrades per turn, it never gates
	// boot — contrast the MCP manager's fail-fast above).
	memSvc := memory.NewService(cfg.Memory)
	if memSvc != nil {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		ok, herr := memSvc.Health(probeCtx)
		probeCancel()
		if herr != nil || !ok {
			log.Warn("openviking memory server not reachable at startup; recall/capture will retry per turn",
				zap.String("base_url", cfg.Memory.BaseURL),
				zap.Bool("healthy", ok),
				zap.Error(herr))
		} else {
			log.Info("openviking memory server healthy",
				zap.String("base_url", cfg.Memory.BaseURL),
				zap.String("account", cfg.Memory.Account))
		}
	}

	// The workspace-root closure maps the authenticated user id to its workspace directory under the data root; the orchestrator's per-request AgentFactory uses the workspace_root passed to Handle, so the closure here is only a convenience accessor for handlers that need it.
	wsFn := func(userID string) string {
		return fsStore.UserWorkspace(userID)
	}
	orch, err := agent.NewOrchestrator(llmClient, cfg, reg, serverTools, skillLoader, wsFn, rt.MySQLStore)
	if err != nil {
		log.Fatal("orchestrator init failed", zap.Error(err))
	}
	orch.WithMCPMarket(rt.MCPMarket)
	orch.SetResourceGuard(rt.ResourceGuard)
	if rt.SchedulerClient != nil {
		management := scheduledcontrol.NewSchedulerManagementClient(rt.SchedulerClient)
		taskTools := scheduledtask.NewTools(management, rt.ResourceSvc, cfg.DefaultModelName())
		if rerr := scheduledtask.RegisterAll(reg, taskTools); rerr != nil {
			log.Fatal("register scheduled task tools failed", zap.Error(rerr))
		}
		orch.EnableScheduledTaskTools([]string{
			scheduledtask.NameCreate, scheduledtask.NameList, scheduledtask.NameGet,
			scheduledtask.NameUpdate, scheduledtask.NamePause, scheduledtask.NameResume,
			scheduledtask.NameDelete, scheduledtask.NameResourcesGet, scheduledtask.NameResourcesPublish,
		})
	}

	orchAdapter := handler.NewOrchestratorAdapter(orch)

	// Turn-run lifecycle (turn-detach-resume): the Redis run state (event
	// log, meta, session claim) is shared across agent processes; the
	// registry is process-local and holds every running turn's cancel. The
	// registry is exposed so Finalize can cancel all running turns within
	// the bounded graceful-shutdown window.
	runMgr := run.NewManager(rt.RedisStore.RunStore(), run.NewRegistry())
	streamHandler := handler.NewMessageStreamHandler(
		sessSvc, msgSvc, titleSvc, compSvc, memSvc, orchAdapter, dataDir, runMgr,
		handler.NewModelSelectionConfig(cfg), cfg.Messages.MaxInputTokensLimit(),
		newScheduledTurnServices(rt, llmClient, titleCfg, defaultMaxContext),
	).
		WithRuntimeStatusStore(rt.RedisStore).
		WithAttachmentStore(rt.MySQLStore).
		WithArtifactService(boot.NewArtifactService(rt))
	turnRunHandler := handler.NewTurnRunHandler(runMgr)
	mcpHandler := handler.NewMCPHandler(reg, serverTools, wsFn, rt.MCPMarket)

	// Standalone-scheduler Runtime (extract-scheduler-service): the agent/all
	// role accepts scheduled executions over the internal RPC and drives them
	// through the SAME turn executor and run registry as interactive chat. The
	// Scheduler process is never embedded; when it is unconfigured this block
	// is inert.
	var scheduledRuntimeStop func(context.Context) error
	if rt.SchedulerClient != nil && internalSrv != nil {
		poll, lease, concurrent, attempts, scan := cfg.ScheduledRuntime.Values()
		runtimeV2, rerr := scheduledruntime.New(scheduledruntime.Dependencies{
			Store:       rt.MySQLStore,
			Occurrences: scheduledruntime.NewSchedulerClient(rt.SchedulerClient),
			Users:       rt.MySQLStore,
			Turns:       streamHandler.TurnExecutor(),
			Canceller:   runCanceller{mgr: runMgr},
			Observer:    runObserver{mgr: runMgr},
			Resources:   rt.ResourceSvc,
			Tokens:      service.NewExecutionTokenIssuer(cfg.JWT.Secret, time.Hour),
			Selection:   handler.NewModelSelectionConfig(cfg),
			Config: scheduledruntime.Config{
				PollInterval: poll, LeaseDuration: lease, MaxConcurrent: concurrent,
				MaxPrepareAttempts: attempts, ScanBatch: scan, DefaultRunDuration: 10 * time.Minute,
			},
		})
		if rerr != nil {
			log.Fatal("scheduled runtime init failed", zap.Error(rerr))
		}
		runtimeV2.Start()
		scheduledruntimev1.RegisterScheduledRuntimeServiceServer(internalSrv, scheduledruntime.NewService(runtimeV2))
		scheduledRuntimeStop = runtimeV2.Shutdown
		log.Info("scheduled execution runtime started", zap.String("role", rt.Role))
	}

	// Session message queue (session-message-queue, messages.queue): default
	// OFF — enablement is confined to this opt-in block. The stop closure is
	// always present because the scheduler shares the same shutdown gate even
	// when the interactive FIFO is disabled.
	var queueReconciler *handler.QueueReconciler
	if cfg.Messages.Queue.Enabled {
		streamHandler.EnableMessageQueue(cfg.Messages.QueueMaxDepthLimit())
		queueReconciler = handler.NewQueueReconciler(streamHandler, 0)
		queueReconciler.Start()
	}
	stopAdmission := func() {
		// Gate new interactive/background turns FIRST so no new turn can cross
		// a startup fence while shutdown drains existing work.
		streamHandler.StopNewTurns()
		if scheduledRuntimeStop != nil {
			ctx, cancel := context.WithTimeout(context.Background(), boot.ShutdownTimeout)
			if err := scheduledRuntimeStop(ctx); err != nil {
				log.Warn("scheduled runtime shutdown wait timed out; proceeding", zap.Error(err))
			}
			cancel()
		}
		if queueReconciler != nil {
			queueReconciler.Close()
			if !streamHandler.WaitForStartupPumps(15 * time.Second) {
				log.Warn("queue pump startups still in flight at shutdown bound; entries may requeue after exit",
					zap.String("role", rt.Role))
			}
		}
	}

	return &Assembly{
		Routes: handler.RouteDeps{
			AuthMW:      middleware.AuthMiddleware(cfg.JWT.Secret),
			SendMessage: streamHandler.SendMessage,
			TurnCancel:  turnRunHandler.CancelTurn,
			TurnEvents:  turnRunHandler.TurnEvents,
			MCPTools:    mcpHandler.Tools,
		},
		MCPManager:    mcpManager,
		RawFlusher:    rawFlusher,
		RunRegistry:   runMgr.Registry,
		Memory:        memSvc,
		stopAdmission: stopAdmission,
	}
}

// newScheduledTurnServices binds every persistence surface of a scheduled turn
// to the same table group while retaining the shared cache, filesystem and LLM.
func newScheduledTurnServices(rt *boot.Runtime, llmClient agent.LLMClient, titleCfg config.OpenAIConfig, defaultMaxContext int) *turn.ScheduledServices {
	scheduledStore := rt.MySQLStore.ForSource(model.SessionSourceScheduled)
	deps := service.SessionDeps{MySQL: scheduledStore, Redis: rt.RedisStore, FS: rt.FSStore}
	sessions := service.NewSessionService(deps)
	return &turn.ScheduledServices{
		Sessions:   sessions,
		Messages:   service.NewMessageService(deps, sessions.SaveMessage),
		Titles:     service.NewTitleService(llmClient, scheduledStore, titleCfg),
		Compaction: service.NewCompactionService(deps, llmClient, rt.Cfg.DefaultModelName(), defaultMaxContext),
		Snapshots:  scheduledStore,
	}
}

// runCanceller adapts the shared run Manager to the scheduled runtime's
// cancellation port: in-process registry first, then the cross-replica flag.
type runCanceller struct{ mgr *run.Manager }

func (c runCanceller) RequestCancel(ctx context.Context, runID string) error {
	if c.mgr.Registry != nil && c.mgr.Registry.Cancel(runID) {
		return nil
	}
	return c.mgr.Store.SetCancelFlag(ctx, runID)
}

// runObserver adapts the shared run Manager to the runtime liveness port.
type runObserver struct{ mgr *run.Manager }

func (o runObserver) Alive(ctx context.Context, runID string) (bool, error) {
	return o.mgr.Store.Alive(ctx, runID)
}

func (o runObserver) InProcess(runID string) bool {
	_, ok := o.mgr.Registry.Get(runID)
	return ok
}

// needsLubanTools reports whether any agent explicitly lists one of the luban skill tools in its tools list.
func needsLubanTools(agents config.AgentsConfig) bool {
	lubanTools := []string{luban.ToolListSkills, luban.ToolReadSkill, luban.ToolInstallSkill, luban.ToolListSkillFiles, luban.ToolTreeSkill}
	for _, cfg := range []config.AgentConfig{agents.Confucius, agents.Subagent.AgentConfig} {
		for _, name := range lubanTools {
			if slices.Contains(cfg.Tools, name) {
				return true
			}
		}
	}
	return false
}

// toServerToolSet converts the server-name -> tool-names mapping into the map[string]map[string]struct{} shape expected by Config.ValidateAgentMCPTools.
func toServerToolSet(serverTools map[string][]string) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(serverTools))
	for serverName, names := range serverTools {
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			set[n] = struct{}{}
		}
		out[serverName] = set
	}
	return out
}
