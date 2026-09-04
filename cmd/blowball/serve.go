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
	"github.com/lush/blowball/internal/llmraw"
	"github.com/lush/blowball/internal/memory"
	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/msgflush"
	"github.com/lush/blowball/internal/pkg/logger"
	"github.com/lush/blowball/internal/run"
	"github.com/lush/blowball/internal/service"
	"github.com/lush/blowball/internal/skillmarket"
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
	// The drain hook exposes the synchronous write-behind drain to the service
	// layer (pre-delete archive completeness and Redis-miss backfill safety);
	// the notify channel wakes the message flusher when the queue crosses its
	// batch threshold. Both roles get the drain — the api role deletes
	// sessions too — while the flusher goroutine below is agent/all only.
	drainMessages := func(ctx context.Context) error {
		return msgflush.Drain(ctx, rt.redisStore, rt.mysqlStore)
	}
	msgNotify := make(chan struct{}, 1)
	sessDeps := service.SessionDeps{
		MySQL:             rt.mysqlStore,
		Redis:             rt.redisStore,
		FS:                rt.fsStore,
		DrainMessageQueue: drainMessages,
		NudgeMessageFlush: func() {
			select {
			case msgNotify <- struct{}{}:
			default:
			}
		},
	}
	sessSvc := service.NewSessionService(sessDeps)

	// Message write-behind flusher (message-write-behind capability): moves
	// the msgs:buffer ingest queue into MySQL in batches. Constructed and
	// started only by the agent/all roles — the api role never runs the
	// consumption path (its SessionHandler is CRUD-only); a nil msgFlusher
	// on the api role skips the shutdown drain below. Start() performs the
	// processing-residue crash recovery before the loop begins.
	var msgFlusher *msgflush.Flusher
	if role != "api" {
		msgFlusher = msgflush.NewFlusher(rt.redisStore, rt.mysqlStore, msgflush.Config{
			Interval:  rt.cfg.Messages.FlushInterval,
			BatchSize: rt.cfg.Messages.FlushBatchSize,
		}, msgNotify)
		msgFlusher.Start()
	}

	// 3. Engine + health check. Each role gets its own engine with the standard
	// middleware chain (Recovery → Trace → CORS); auth is applied per-route-group
	// inside the registration functions.
	engine := newEngine()
	handler.RegisterHealthz(engine)

	// 4. Register routes by role. wireAPI/wireAgent each construct only the
	// handlers their partition needs, so the api role never instantiates the
	// orchestrator, OpenAI client, tool registry, or MCP manager.
	var mcpMgr *mcpclient.Manager
	var rawFlusher *llmraw.Flusher
	var runReg *run.Registry
	var memSvc *memory.Service
	switch role {
	case "all":
		handler.RegisterAPIRoutes(engine, wireAPI(rt, sessSvc))
		agentDeps, mgr, fl, reg, mem := wireAgent(rt, sessSvc)
		mcpMgr = mgr
		rawFlusher = fl
		runReg = reg
		memSvc = mem
		handler.RegisterAgentRoutes(engine, agentDeps)
	case "api":
		handler.RegisterAPIRoutes(engine, wireAPI(rt, sessSvc))
	case "agent":
		agentDeps, mgr, fl, reg, mem := wireAgent(rt, sessSvc)
		mcpMgr = mgr
		rawFlusher = fl
		runReg = reg
		memSvc = mem
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

	// Cancel every running turn (turn-detach-resume) so graceful shutdown is
	// bounded: each cancelled turn persists its partial output through the
	// ordinary interrupted-turn path and finalizes its run state. Since
	// harden-turn-persistence the terminal message batch (bounded retry +
	// pre-release drain) runs ON the turn goroutine before Finish, so
	// WaitAll below covers persistence completion directly — no detached
	// goroutine grace-period heuristic is needed. RunKeyTTL still backstops
	// anything that misses the shutdown window.
	if runReg != nil {
		if n := runReg.CancelAll(); n > 0 {
			log.Info("cancelling running turns for shutdown", zap.Int("turns", n))
			wctx, wcancel := context.WithTimeout(context.Background(), ShutdownTimeout)
			if err := runReg.WaitAll(wctx); err != nil {
				log.Warn("shutdown turn wait timed out; proceeding", zap.Error(err))
			}
			wcancel()
		}
	}

	// Drain the raw-capture write-behind buffer before the deferred store
	// closes release Redis/MySQL, so a graceful restart does not leave the
	// tail of the buffer unflushed (nil on the api role, which never captures).
	if rawFlusher != nil {
		rawFlusher.Close()
		log.Info("llm raw flusher stopped", zap.String("role", role))
	}
	// Same for the message write-behind queue: Close stops the loop and runs
	// one bounded final drain so a normal release does not leave the turn
	// tail queued (nil on the api role, which never runs the flusher).
	if msgFlusher != nil {
		msgFlusher.Close()
		log.Info("message flusher stopped", zap.String("role", role))
	}
	// Release the memory client's idle keep-alive connections last (nil on a
	// disabled config). In-flight captures are not aborted: whatever landed
	// on the OpenViking session stays pending and the session's next turn
	// commits it — at-least-once.
	if memSvc != nil {
		memSvc.Close()
	}
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
	marketDir  string
	log        *zap.Logger
	mysqlStore *mysql.Store
	redisStore *redis.Store
	fsStore    *fs.Store
	// market is the skill-market client (skill-market capability), nil when
	// the skill_market config block is off. Constructed here in the shared
	// setup because BOTH roles consume it: the api role for the skills list
	// endpoint, the agent role for the luban tools and the bash sandbox's
	// per-skill market mounts.
	market *skillmarket.Client
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

	// Derive the five runtime locations from the single -d root (D2/D3/D6): data, logs, skills, tools, skills-market.
	dataDir := filepath.Join(dataRoot, "data")
	logDir := filepath.Join(dataRoot, "logs")
	skillsDir := filepath.Join(dataRoot, "skills")
	toolsDir := filepath.Join(dataRoot, "tools")
	marketDir := filepath.Join(dataRoot, "skills-market")

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
		marketDir: marketDir,
		log:       log,
	}

	// Skill-market client (skill-market capability): nil for a disabled
	// config, constructed here in the shared setup because both roles consume
	// it. Pure HTTP client holder — no store dependencies, no startup probe
	// (failures degrade per call, never gate boot).
	rt.market = skillmarket.New(cfg.SkillMarket, marketDir)

	log.Info("runtime layout",
		zap.String("role", role),
		zap.String("config", configPath),
		zap.String("data_root", dataRoot),
		zap.String("data_dir", dataDir),
		zap.String("log_dir", logDir),
		zap.String("skills_dir", skillsDir),
		zap.String("tools_dir", toolsDir),
		zap.String("skills_market_dir", marketDir),
		zap.Bool("skill_market_enabled", rt.market.Enabled()))

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

	// Best-effort Redis AOF probe (message-write-behind deployment
	// prerequisite): the msgs:buffer ingest queue is the only synchronous
	// layer for messages, so a Redis restart without AOF loses whatever sits
	// in the flush window. A probe failure (no CONFIG permission, managed
	// Redis) is skipped silently — only a definitive "off" WARNs; startup is
	// never blocked.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	aofOn, aofErr := redisStore.AppendOnlyEnabled(probeCtx)
	probeCancel()
	if aofErr != nil {
		log.Debug("redis appendonly probe failed; skipping", zap.Error(aofErr))
	} else if !aofOn {
		log.Warn("redis appendonly is disabled; messages sitting in the write-behind flush window are lost on a redis restart — enable AOF (appendonly yes)")
	}

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

	// Ensure the operator tools directory exists (always created, even when empty) so the landlock rule and the in-sandbox --ro-bind always resolve. Operators place CLI binaries here to expose them inside the bash sandbox at $HOME/.local/bin.
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		log.Fatal("create tools dir failed", zap.Error(err))
	}

	// Ensure the skills-market directory exists (skill-market capability), in
	// the same always-create-even-empty sequence as tools: operators sync
	// market skill payloads into {market_uid}/{skill_name}/ here; an empty
	// directory is harmless (no mounts, no allowlist entries — visibility
	// comes only from the market API). Like tools, it lives directly under -d
	// (NOT inside data/), so it never participates in the shared-storage
	// health check or the FUSE anchor; multi-host deployments sync it per
	// host.
	if err := os.MkdirAll(marketDir, 0o755); err != nil {
		log.Fatal("create skills-market dir failed", zap.Error(err))
	}

	// go-landlock (D5/D6). The runtime subdirs the process writes to (data/logs/skills) are restricted read-write — covering logs for lumberjack's post-rotation reopen — plus operator extra_read_write; the operator tools dir and the skills-market dir are restricted read-only (the agent never writes market payloads — operators sync them), plus operator extra_read_only; the configurable system_read_only baseline is restricted read-only too. Best-effort: a no-op on non-Linux platforms and logged at warn rather than fatal so macOS dev workflows keep running. The application-layer path validation in xizhi still enforces per-user workspace isolation regardless. landlock.enabled: false skips ApplyLandlock entirely (warning-only). All defaults reproduce the pre-configurability literals.
	rwDirs := append([]string{dataDir, logDir, skillsDir}, cfg.Landlock.ExtraReadWrite...)
	roDirs := append([]string{toolsDir, marketDir}, cfg.Landlock.ExtraReadOnly...)
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
	// The title config carries the wiring-resolved title model (title_model
	// or the default catalog entry) — see TitleModelName.
	titleCfg := cfg.OpenAI
	titleCfg.TitleModel = cfg.TitleModelName()
	titleSvc := service.NewTitleService(nil, rt.mysqlStore, titleCfg)
	// The run store feeds the session list's generating flag; the api role
	// reads the shared Redis claims but never constructs a run registry.
	sessionHandler := handler.NewSessionHandler(sessSvc, titleSvc, rt.redisStore.RunStore())
	workspaceHandler := handler.NewWorkspaceHandler(rt.fsStore, MaxUploadBytes, handler.OnlyOfficeSettings{
		Secret:            cfg.OnlyOffice.Secret,
		ServerURL:         cfg.OnlyOffice.ServerURL,
		InternalBackend:   cfg.OnlyOffice.InternalBackend,
		VersionServiceURL: cfg.OnlyOffice.VersionServiceURL,
	})
	skillHandler := handler.NewSkillHandler(rt.fsStore, rt.market)
	modelListHandler := handler.NewModelListHandler(cfg.ModelCatalog(), cfg.DefaultModelName(), cfg.OpenAI.DefaultReasoningEffort)

	return handler.RouteDeps{
		AuthMW:                           middleware.AuthMiddleware(cfg.JWT.Secret),
		QueryTokenAuthMW:                 middleware.QueryTokenAuthMiddleware(cfg.JWT.Secret),
		Login:                            authHandler.Login,
		SessionList:                      sessionHandler.ListSessions,
		SessionCreate:                    sessionHandler.CreateSession,
		SessionGet:                       sessionHandler.GetSession,
		SessionMessages:                  sessionHandler.GetSessionMessages,
		SessionDelete:                    sessionHandler.DeleteSession,
		SessionUpdateTitle:               sessionHandler.UpdateTitle,
		WorkspaceList:                    workspaceHandler.List,
		WorkspaceUpload:                  workspaceHandler.Upload,
		WorkspaceSearch:                  workspaceHandler.Search,
		WorkspaceDownload:                workspaceHandler.Download,
		WorkspaceTokenDownload:           workspaceHandler.TokenDownload,
		WorkspaceContent:                 workspaceHandler.Content,
		WorkspaceWriteContent:            workspaceHandler.WriteContent,
		WorkspaceDelete:                  workspaceHandler.Delete,
		WorkspaceRename:                  workspaceHandler.Rename,
		WorkspaceCreate:                  workspaceHandler.Create,
		WorkspaceOnlyOfficeConfig:        workspaceHandler.OnlyOfficeConfig,
		WorkspaceOnlyOfficeVersionConfig: workspaceHandler.OnlyOfficeVersionConfig,
		WorkspaceOnlyOfficeCallback:      workspaceHandler.OnlyOfficeCallback,
		SkillsList:                       skillHandler.List,
		ModelsList:                       modelListHandler.List,
	}
}

// wireAgent builds the agent layer for the agent role (and contributes it in
// the all role): the tool registry, the external MCP manager, the OpenAI
// client (with raw-capture sink attached), the orchestrator, the title
// service, and the streaming + MCP-tool handlers. It returns a RouteDeps
// populated with only the agent-route handlers (SendMessage, MCPTools) plus
// the auth middleware, the MCP manager so serveRun can defer its Close, and
// the raw-capture flusher so serveRun can drain it on shutdown.
func wireAgent(rt *appRuntime, sessSvc *service.SessionService) (handler.RouteDeps, *mcpclient.Manager, *llmraw.Flusher, *run.Registry, *memory.Service) {
	cfg := rt.cfg
	dataDir := rt.dataDir
	fsStore := rt.fsStore
	log := rt.log

	// Tool registry. The main registry backs the MCP tools-listing endpoint. Real tool execution during orchestration uses a per-request registry the orchestrator's factory rebuilds scoped to the user's workspace root.
	reg := tool.NewRegistry()
	reg.SetTimeouts(cfg.Tools.Timeouts)
	xizhi.RegisterAll(reg, dataDir, cfg.Tools.Xizhi)

	// Sandboxed bash execution. Only registered on Linux where bwrap is available; on other platforms the enabled tool is ignored. If bash is explicitly enabled but bwrap is missing on Linux, startup fails fast. (The dedicated python/pip_install executors were removed; Python code and pip installs run via bash.)
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
		}, rt.skillsDir, rt.toolsDir).WithMarket(rt.market)
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
		}).WithMarket(rt.market)
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

	// Raw LLM capture (llm-raw-capture capability): the sink stages captured
	// request/response/error payloads in the Redis write-behind buffer and the
	// flusher batch-inserts them into llm_raw_log. Zero-config, always on for
	// agent/all roles; serveRun closes the flusher after the HTTP shutdown so
	// the buffer drains before the stores are released.
	rawNotify := make(chan struct{}, 1)
	rawSink := llmraw.NewSink(rt.redisStore, rawNotify)
	rawFlusher := llmraw.NewFlusher(rt.redisStore, rt.mysqlStore, rawNotify)
	rawFlusher.Start()

	openAIClient := agent.NewOpenAIClientWithSink(cfg.OpenAI, rawSink)
	var webfetchDigester webfetch.ContentDigester
	if cfg.Tools.Webfetch.Digest.Enabled {
		promptClient, err := agent.NewWebfetchPromptClient(openAIClient, cfg.OpenAI, cfg.Tools.Webfetch.Digest.Model)
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

	// Title generation runs on its own resolved model (openai.title_model or
	// the default catalog entry) over the shared client.
	titleCfg := cfg.OpenAI
	titleCfg.TitleModel = cfg.TitleModelName()
	titleSvc := service.NewTitleService(openAIClient, rt.mysqlStore, titleCfg)

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
		service.SessionDeps{MySQL: rt.mysqlStore, Redis: rt.redisStore, FS: fsStore},
		openAIClient,
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
	orch, err := agent.NewOrchestrator(openAIClient, cfg, reg, serverTools, skillLoader, wsFn, rt.mysqlStore)
	if err != nil {
		log.Fatal("orchestrator init failed", zap.Error(err))
	}

	orchAdapter := handler.NewOrchestratorAdapter(orch)

	// Turn-run lifecycle (turn-detach-resume): the Redis run state (event
	// log, meta, session claim) is shared across agent processes; the
	// registry is process-local and holds every running turn's cancel. The
	// registry is returned so serveRun can cancel all running turns within
	// the bounded graceful-shutdown window.
	runMgr := run.NewManager(rt.redisStore.RunStore(), run.NewRegistry())
	streamHandler := handler.NewMessageStreamHandler(sessSvc, msgSvc, titleSvc, compSvc, memSvc, orchAdapter, dataDir, runMgr, handler.NewModelSelectionConfig(cfg), cfg.Messages.MaxInputTokensLimit())
	turnRunHandler := handler.NewTurnRunHandler(runMgr)
	mcpHandler := handler.NewMCPHandler(reg, serverTools, wsFn)

	return handler.RouteDeps{
		AuthMW:      middleware.AuthMiddleware(cfg.JWT.Secret),
		SendMessage: streamHandler.SendMessage,
		TurnCancel:  turnRunHandler.CancelTurn,
		TurnEvents:  turnRunHandler.TurnEvents,
		MCPTools:    mcpHandler.Tools,
	}, mcpManager, rawFlusher, runMgr.Registry, memSvc
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

// executorConfigured reports whether the sandboxed bash executor tool is
// enabled in config. It gates both executor registration in wireAgent and the
// shared-mode bwrap self-check in setupRuntime. (The dedicated python/pip_install
// executors were removed; Python and pip run via bash.)
func executorConfigured(cfg *config.Config) bool {
	return cfg.Tools.Executor.Bash.Enabled
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
