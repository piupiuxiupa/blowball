// Package main is the blowball unified CLI entry point.
//
// The cobra root exposes `serve` and `seed` subcommands and persistent `-f`/`--config` and
// `-d`/`--data-dir` flags. See main.go for the command wiring; this file holds the
// `serve` subcommand (HTTP server bootstrap) and seed.go holds the `seed` subcommand.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/lush/blowball/internal/agent"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/handler"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/storage"
	"github.com/lush/blowball/internal/store/fs"
	"github.com/lush/blowball/internal/store/mysql"
	"github.com/lush/blowball/internal/store/redis"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/executor"
	"github.com/lush/blowball/internal/tool/luban"
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/skill"
	"github.com/lush/blowball/internal/tool/webfetch"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// RedisCacheTTL is the expiration applied to every session-level cache write.
// The spec defaults to 24h; if the deployment wants a different value it can be surfaced
// through config later without touching this constant.
const RedisCacheTTL = 24 * time.Hour

// MaxUploadBytes caps a single multipart upload at 50 MiB. Larger uploads are rejected with 413 before they reach disk.
const MaxUploadBytes = 50 << 20

// ShutdownTimeout is the upper bound on draining in-flight requests after a SIGINT/SIGTERM,
// per the api-server spec's graceful-shutdown requirement.
const ShutdownTimeout = 10 * time.Second

// validRoles is the set of accepted --role values. "all" is the default and
// preserves the pre-split single-process behavior; "api" and "agent" select the
// partitioned process roles (see the service-roles spec).
var validRoles = []string{"all", "api", "agent"}

// newServeCmd builds the `serve` cobra subcommand. It runs the HTTP server
// bootstrap, deriving the runtime data root from the persistent -d flag, the
// config path from -f, and the process role from --role.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Run the blowball HTTP server",
		Long:         "Run the blowball HTTP server (Gin) with graceful shutdown on SIGINT/SIGTERM.",
		SilenceUsage: true,
		RunE:         serveRun,
	}
	// --role selects which route partition this process serves. "all" (default)
	// is the rollback path: one process, full route set, single listener on
	// server.port — identical to the pre-split monolith.
	cmd.Flags().String("role", "all", "process role: all|api|agent")
	return cmd
}

// serveRun is the server bootstrap. Bootstrap order (see design.md D3):
//
//  0. resolve --role, -f, -d from cobra flags
//  1. shared setup (setupRuntime): config → runtime dirs → logger (role-aware
//     filename) → MySQL/Redis/FS → skills/tools dirs → Landlock. Plus the
//     role-aware openai.api_key requirement.
//  2. build the shared (store-only) SessionService both roles need
//  3. build the engine (Recovery → Trace → CORS) and mount /healthz
//  4. register routes by role: wireAPI (CRUD) and/or wireAgent (streaming + MCP)
//  5. per-role HTTP listener + graceful shutdown
func serveRun(cmd *cobra.Command, _ []string) error {
	// Validate --role before any setup so a bad value exits non-zero without
	// touching the filesystem or opening connections.
	role, err := resolveRole(cmd)
	if err != nil {
		return err
	}
	configPath, dataRoot, err := persistentFlags(cmd)
	if err != nil {
		return err
	}

	// 1. Shared setup (runs for every role).
	rt, err := setupRuntime(configPath, dataRoot, role)
	if err != nil {
		return err
	}
	log := rt.log
	defer func() { _ = log.Sync() }()
	defer func() {
		if cerr := rt.mysqlStore.Close(); cerr != nil {
			log.Warn("mysql close failed", zap.Error(cerr))
		}
	}()
	defer func() {
		if cerr := rt.redisStore.Close(); cerr != nil {
			log.Warn("redis close failed", zap.Error(cerr))
		}
	}()

	// 2. Shared session service. Store-only, so it carries no agent-layer
	// dependency; both the api CRUD handlers and the agent streaming handler use it.
	sessDeps := service.SessionDeps{MySQL: rt.mysqlStore, Redis: rt.redisStore, FS: rt.fsStore}
	sessSvc := service.NewSessionService(sessDeps)

	// 3. Engine + health check. Each role gets its own engine with the standard
	// middleware chain (Recovery → Trace → CORS); auth is applied per-route-group
	// inside the registration functions.
	engine := newEngine()
	handler.RegisterHealthz(engine)

	// 4. Register routes by role. wireAPI/wireAgent each construct only the
	// handlers their partition needs, so the api role never instantiates the
	// orchestrator, OpenAI client, tool registry, or MCP manager.
	var mcpMgr *mcpclient.Manager
	switch role {
	case "all":
		handler.RegisterAPIRoutes(engine, wireAPI(rt, sessSvc))
		agentDeps, mgr := wireAgent(rt, sessSvc)
		mcpMgr = mgr
		handler.RegisterAgentRoutes(engine, agentDeps)
	case "api":
		handler.RegisterAPIRoutes(engine, wireAPI(rt, sessSvc))
	case "agent":
		agentDeps, mgr := wireAgent(rt, sessSvc)
		mcpMgr = mgr
		handler.RegisterAgentRoutes(engine, agentDeps)
	}
	if mcpMgr != nil {
		defer func() {
			if cerr := mcpMgr.Close(); cerr != nil {
				log.Warn("mcp client close failed", zap.Error(cerr))
			}
		}()
	}

	// 5. Per-role listener. all and api listen on server.port; agent listens on
	// server.agent_port. D2: all keeps a single listener on server.port so any
	// existing caller (tests, dev proxy, monitoring) keeps working unchanged.
	port := rt.cfg.Server.Port
	if role == "agent" {
		port = rt.cfg.Server.AgentPort
	}

	log.Info("server starting",
		zap.String("role", role),
		zap.Int("port", port),
		zap.Strings("route_groups", routeGroups(role)))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: engine,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down server", zap.String("role", role))

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("server shutdown error", zap.Error(err))
	}
	log.Info("server stopped", zap.String("role", role))
	return nil
}

// runtime bundles everything the shared setup produces and both role builders
// consume: the parsed config, the selected role, the derived runtime
// directories, the zap logger, and the three shared stores. Stores are concrete
// because they are constructed once at startup and closed via deferred Close in
// serveRun.
type appRuntime struct {
	cfg        *config.Config
	role       string
	dataDir    string
	logDir     string
	skillsDir  string
	toolsDir   string
	log        *zap.Logger
	mysqlStore *mysql.Store
	redisStore *redis.Store
	fsStore    *fs.Store
}

// setupRuntime performs the shared bootstrap that runs for every role: load
// config, derive the four runtime directories, init the role-aware logger,
// enforce the role-aware openai.api_key requirement, connect MySQL/Redis, open
// the FS store, ensure the skills/tools dirs exist, and apply Landlock. It
// matches the pre-split bootstrap step-for-step; the only additions are the
// role-scoped log filename and the relaxed openai-key check for the api role.
//
// Store-init failures use log.Fatal (as before) so a bad DSN or unreachable
// Redis aborts startup with a clear message rather than a generic cobra error.
func setupRuntime(configPath, dataRoot, role string) (*appRuntime, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", configPath, err)
	}

	// Derive the four runtime locations from the single -d root (D2/D3/D6): data, logs, skills, tools.
	dataDir := filepath.Join(dataRoot, "data")
	logDir := filepath.Join(dataRoot, "logs")
	skillsDir := filepath.Join(dataRoot, "skills")
	toolsDir := filepath.Join(dataRoot, "tools")

	// Ensure the log directory exists before the logger opens a file in it (D8 fail-fast is enforced inside logger.Init too).
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", logDir, err)
	}

	// Init the zap logger with a role-scoped filename so two role processes on
	// the same host do not contend on one lumberjack-managed file.
	log, err := logger.InitForRole(cfg.Logging, logDir, role)
	if err != nil {
		return nil, fmt.Errorf("init logger: %w", err)
	}

	// Role-aware openai.api_key requirement. The api role never drives the LLM,
	// so a missing key is downgraded to a warning; the all and agent roles
	// construct the orchestrator/title service and therefore require it.
	if strings.TrimSpace(cfg.OpenAI.APIKey) == "" {
		if openAIKeyRequired(role) {
			log.Fatal("openai.api_key must be non-empty for this role",
				zap.String("role", role))
		}
		log.Warn("openai.api_key is empty; allowed because this role never calls the LLM",
			zap.String("role", role))
	}

	rt := &appRuntime{
		cfg:       cfg,
		role:      role,
		dataDir:   dataDir,
		logDir:    logDir,
		skillsDir: skillsDir,
		toolsDir:  toolsDir,
		log:       log,
	}

	log.Info("runtime layout",
		zap.String("role", role),
		zap.String("config", configPath),
		zap.String("data_root", dataRoot),
		zap.String("data_dir", dataDir),
		zap.String("log_dir", logDir),
		zap.String("skills_dir", skillsDir),
		zap.String("tools_dir", toolsDir))

	// MySQL. sqlx.Connect pings on construction so a bad DSN fails fast.
	mysqlStore, err := mysql.New(cfg.MySQL.DSN)
	if err != nil {
		log.Fatal("mysql init failed", zap.Error(err))
	}
	rt.mysqlStore = mysqlStore

	// Redis. The shared TTL is applied to session-level cache keys.
	redisStore, err := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, RedisCacheTTL)
	if err != nil {
		log.Fatal("redis init failed", zap.Error(err))
	}
	rt.redisStore = redisStore

	// FS store for per-user session files, workspace and skills directories. fs.New creates dataDir.
	fsStore, err := fs.New(dataDir)
	if err != nil {
		log.Fatal("fs store init failed", zap.Error(err))
	}
	rt.fsStore = fsStore

	// Shared POSIX filesystem backend startup health check (see the
	// workspace-shared-storage spec). When storage.workspace.backend == "shared",
	// {data-dir}/data MUST be the operator-mounted shared FS (MinIO-backed
	// JuiceFS). The check runs BEFORE Landlock for every role (the api role does
	// workspace CRUD on the shared data plane too) and fatals on a missing/wrong
	// mount so a node can never silently degrade to local disk and diverge from
	// the cluster. When executor tools are configured and this role runs them
	// (agent/all), an extra bwrap probe catches a missing JuiceFS --allow-other.
	if cfg.Storage.Workspace.IsShared() {
		log.Info("shared workspace backend enabled; running mount health check",
			zap.String("data_dir", dataDir))
		if err := storage.CheckSharedBackend(storage.CheckOptions{DataDir: dataDir, Log: log}); err != nil {
			log.Fatal("shared workspace backend health check failed; refusing to start",
				zap.Error(err),
				zap.String("remediation", "mount JuiceFS onto {data-dir}/data (see docs/shared-storage.md) before starting blowball"))
		}
		if executorConfigured(cfg) && role != "api" {
			if err := executor.ProbeFUSEWorkspace(dataDir); err != nil {
				log.Fatal("executor shared-workspace self-check failed; refusing to start", zap.Error(err))
			}
		}
	}

	// Ensure the global skills directory exists (the loader does not create it) so per-subdir landlock below resolves cleanly.
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		log.Fatal("create skills dir failed", zap.Error(err))
	}

	// Ensure the operator tools directory exists (always created, even when empty) so the landlock rule and the in-sandbox --ro-bind always resolve. Operators place CLI binaries here to expose them inside the bash/python/pip sandboxes at $HOME/.local/bin.
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		log.Fatal("create tools dir failed", zap.Error(err))
	}

	// go-landlock (D5/D6). The runtime subdirs the process writes to (data/logs/skills) are restricted read-write — covering logs for lumberjack's post-rotation reopen — plus operator extra_read_write; the operator tools dir is restricted read-only, plus operator extra_read_only; the configurable system_read_only baseline is restricted read-only too. Best-effort: a no-op on non-Linux platforms and logged at warn rather than fatal so macOS dev workflows keep running. The application-layer path validation in xizhi still enforces per-user workspace isolation regardless. landlock.enabled: false skips ApplyLandlock entirely (warning-only). All defaults reproduce the pre-configurability literals.
	rwDirs := append([]string{dataDir, logDir, skillsDir}, cfg.Landlock.ExtraReadWrite...)
	roDirs := append([]string{toolsDir}, cfg.Landlock.ExtraReadOnly...)
	log.Info("landlock policy",
		zap.Bool("enabled", cfg.Landlock.IsEnabled()),
		zap.Strings("rw_dirs", rwDirs),
		zap.Strings("ro_dirs", roDirs),
		zap.Strings("system_read_only", cfg.Landlock.SystemReadOnly),
		zap.Strings("extra_read_only_mounts", sandboxMountTargets(cfg.Tools.Executor.Sandbox.ExtraReadOnlyMounts)),
		zap.Strings("extra_read_write_mounts", sandboxMountTargets(cfg.Tools.Executor.Sandbox.ExtraReadWriteMounts)))
	if cfg.Landlock.IsEnabled() {
		// Guard 2.1 (≥1 effective RW dir) is a config-invalid condition → refuse to
		// start, distinct from a kernel landlock failure below which is best-effort.
		if err := config.ValidateLandlockRW(true, []string{dataDir, logDir, skillsDir}, cfg.Landlock.ExtraReadWrite); err != nil {
			log.Fatal("landlock config invalid; refusing to start", zap.Error(err))
		}
		if err := xizhi.ApplyLandlock(rwDirs, roDirs, cfg.Landlock.SystemReadOnly); err != nil {
			log.Warn("landlock not applied; relying on application-layer validation only",
				zap.Error(err))
		}
	} else {
		log.Warn("landlock disabled by config (landlock.enabled: false); relying on application-layer validation only")
	}

	return rt, nil
}

// sandboxMountTargets returns the in-sandbox target paths of the given extra
// mounts for the startup audit log. It lives here (rather than on the config
// type) to keep the parsed MountSpec export minimal.
func sandboxMountTargets(mounts []config.MountSpec) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Host+":"+m.Target)
	}
	return out
}

// wireAPI builds the CRUD services/handlers for the api role (and contributes
// them in the all role): auth, session CRUD, message-history read, manual title
// update, workspace file CRUD, and the skills list. It returns a RouteDeps
// populated with only the API-route handlers plus the auth middleware; the
// agent-partition fields (SendMessage, MCPTools) are left nil.
//
// Fault isolation: wireAPI does NOT construct the orchestrator, OpenAI client,
// tool registry, or MCP manager. The api role's TitleService is built with a
// nil LLM client — SetManualTitle never calls the LLM, so the api role needs no
// OpenAI dependency.
func wireAPI(rt *appRuntime, sessSvc *service.SessionService) handler.RouteDeps {
	cfg := rt.cfg

	jwtExpire, err := cfg.JWT.ParseDuration()
	if err != nil {
		rt.log.Fatal("parse jwt.expire failed", zap.Error(err))
	}
	authSvc := service.NewAuthService(rt.mysqlStore, cfg.JWT.Secret, jwtExpire, cfg.Auth.IsPasswordRequired())
	authHandler := handler.NewAuthHandler(authSvc)

	// The api role never calls the LLM; a nil client is safe because
	// SetManualTitle only touches MySQL (see TitleService.SetManualTitle).
	titleSvc := service.NewTitleService(nil, rt.mysqlStore, cfg.OpenAI)
	sessionHandler := handler.NewSessionHandler(sessSvc, titleSvc)
	workspaceHandler := handler.NewWorkspaceHandler(rt.fsStore, MaxUploadBytes, handler.OnlyOfficeSettings{
		Secret:          cfg.OnlyOffice.Secret,
		ServerURL:       cfg.OnlyOffice.ServerURL,
		InternalBackend: cfg.OnlyOffice.InternalBackend,
	})
	skillHandler := handler.NewSkillHandler(rt.fsStore)

	return handler.RouteDeps{
		AuthMW:                      middleware.AuthMiddleware(cfg.JWT.Secret),
		QueryTokenAuthMW:            middleware.QueryTokenAuthMiddleware(cfg.JWT.Secret),
		Login:                       authHandler.Login,
		SessionList:                 sessionHandler.ListSessions,
		SessionCreate:               sessionHandler.CreateSession,
		SessionMessages:             sessionHandler.GetSessionMessages,
		SessionDelete:               sessionHandler.DeleteSession,
		SessionUpdateTitle:          sessionHandler.UpdateTitle,
		WorkspaceList:               workspaceHandler.List,
		WorkspaceUpload:             workspaceHandler.Upload,
		WorkspaceDownload:           workspaceHandler.Download,
		WorkspaceTokenDownload:      workspaceHandler.TokenDownload,
		WorkspaceContent:            workspaceHandler.Content,
		WorkspaceWriteContent:       workspaceHandler.WriteContent,
		WorkspaceDelete:             workspaceHandler.Delete,
		WorkspaceRename:             workspaceHandler.Rename,
		WorkspaceCreate:             workspaceHandler.Create,
		WorkspaceOnlyOfficeConfig:   workspaceHandler.OnlyOfficeConfig,
		WorkspaceOnlyOfficeCallback: workspaceHandler.OnlyOfficeCallback,
		SkillsList:                  skillHandler.List,
	}
}

// wireAgent builds the agent layer for the agent role (and contributes it in
// the all role): the tool registry, the external MCP manager, the OpenAI
// client, the orchestrator, the title service, and the streaming + MCP-tool
// handlers. It returns a RouteDeps populated with only the agent-route
// handlers (SendMessage, MCPTools) plus the auth middleware, and the MCP
// manager so serveRun can defer its Close.
func wireAgent(rt *appRuntime, sessSvc *service.SessionService) (handler.RouteDeps, *mcpclient.Manager) {
	cfg := rt.cfg
	dataDir := rt.dataDir
	fsStore := rt.fsStore
	log := rt.log

	// Tool registry. The main registry backs the MCP tools-listing endpoint. Real tool execution during orchestration uses a per-request registry the orchestrator's factory rebuilds scoped to the user's workspace root.
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, dataDir, cfg.Tools.Xizhi)
	webfetch.RegisterAll(reg, cfg.Tools.Webfetch)

	// Sandboxed bash/python/pip execution. Only registered on Linux where bwrap is available; on other platforms enabled tools are ignored. If a tool is explicitly enabled but bwrap is missing on Linux, startup fails fast.
	if executorConfigured(cfg) {
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
		}, rt.skillsDir, rt.toolsDir)
		if err := executor.RegisterAll(reg, executorTools); err != nil {
			log.Fatal("register executor tools failed", zap.Error(err))
		}
	}

	// Skill loader. Discover skills from the global skills directory and per-user data/{userID}/skills/ directories. Register the luban skill tools globally when at least one agent lists them.
	skillLoader := skill.NewLoader(rt.skillsDir, func(userID string) string {
		return fsStore.UserSkills(userID)
	})
	if needsLubanTools(cfg.Agents) {
		lubanTools := luban.NewTools(skillLoader, func(userID string) string {
			return fsStore.UserSkills(userID)
		})
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
	msgSvc := service.NewMessageService(service.SessionDeps{MySQL: rt.mysqlStore, Redis: rt.redisStore, FS: fsStore}, sessSvc.SaveMessage)

	openAIClient := agent.NewOpenAIClient(cfg.OpenAI)
	titleSvc := service.NewTitleService(openAIClient, rt.mysqlStore, cfg.OpenAI)

	// The workspace-root closure maps the authenticated user id to its workspace directory under the data root; the orchestrator's per-request AgentFactory uses the workspace_root passed to Handle, so the closure here is only a convenience accessor for handlers that need it.
	wsFn := func(userID string) string {
		return fsStore.UserWorkspace(userID)
	}
	orch, err := agent.NewOrchestrator(openAIClient, cfg, reg, serverTools, skillLoader, wsFn)
	if err != nil {
		log.Fatal("orchestrator init failed", zap.Error(err))
	}

	orchAdapter := handler.NewOrchestratorAdapter(orch)
	streamHandler := handler.NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, orchAdapter, dataDir)
	mcpHandler := handler.NewMCPHandler(reg)

	return handler.RouteDeps{
		AuthMW:      middleware.AuthMiddleware(cfg.JWT.Secret),
		SendMessage: streamHandler.SendMessage,
		MCPTools:    mcpHandler.Tools,
	}, mcpManager
}

// newEngine builds a gin.Engine with the standard middleware chain shared by
// every role: Recovery (panic safety) → Trace (per-request trace_id) → CORS.
// Auth is applied per-route-group inside RegisterAPIRoutes / RegisterAgentRoutes
// so it stays the final middleware before the handler.
func newEngine() *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.TraceMiddleware())
	engine.Use(middleware.CORS())
	return engine
}

// resolveRole reads and validates the --role flag, rejecting unknown values
// before any setup runs so the process exits non-zero without side effects.
func resolveRole(cmd *cobra.Command) (string, error) {
	role, err := cmd.Flags().GetString("role")
	if err != nil {
		return "", fmt.Errorf("read --role: %w", err)
	}
	if !slices.Contains(validRoles, role) {
		return "", fmt.Errorf("invalid --role %q (want %s)", role, strings.Join(validRoles, "|"))
	}
	return role, nil
}

// openAIKeyRequired reports whether the role needs a configured openai.api_key
// at startup. The api role never drives the LLM, so it does not require one;
// the all and agent roles construct the orchestrator and title service, which do.
func openAIKeyRequired(role string) bool {
	return role != "api"
}

// routeGroups returns the human-readable names of the route partitions a role
// registers, for the startup log line.
func routeGroups(role string) []string {
	switch role {
	case "api":
		return []string{"api"}
	case "agent":
		return []string{"agent"}
	default:
		return []string{"api", "agent"}
	}
}

// executorConfigured reports whether any sandboxed executor tool (bash/python/
// pip) is enabled in config. It gates both executor registration in wireAgent
// and the shared-mode bwrap self-check in setupRuntime.
func executorConfigured(cfg *config.Config) bool {
	return cfg.Tools.Executor.Bash.Enabled || cfg.Tools.Executor.Python.Enabled || cfg.Tools.Executor.Pip.Enabled
}

// needsLubanTools reports whether any agent explicitly lists one of the luban skill tools in its tools list.
func needsLubanTools(agents config.AgentsConfig) bool {
	lubanTools := []string{luban.ToolListSkills, luban.ToolReadSkill, luban.ToolInstallSkill}
	for _, cfg := range []config.AgentConfig{agents.Confucius, agents.Chongzhi, agents.Liang} {
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

// persistentFlags resolves the shared -f/--config and -d/--data-dir persistent flags from cmd.
func persistentFlags(cmd *cobra.Command) (configPath, dataRoot string, err error) {
	configPath, err = cmd.Flags().GetString("config")
	if err != nil {
		return "", "", fmt.Errorf("read --config: %w", err)
	}
	dataRoot, err = cmd.Flags().GetString("data-dir")
	if err != nil {
		return "", "", fmt.Errorf("read --data-dir: %w", err)
	}
	return configPath, dataRoot, nil
}
