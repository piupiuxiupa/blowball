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

// newServeCmd builds the `serve` cobra subcommand. It runs the HTTP server bootstrap, deriving the runtime data root from the persistent `-d` flag and the config path from `-f`.
func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "serve",
		Short:        "Run the blowball HTTP server",
		Long:         "Run the blowball HTTP server (Gin) with graceful shutdown on SIGINT/SIGTERM.",
		SilenceUsage: true,
		RunE:         serveRun,
	}
}

// serveRun is the server bootstrap. Bootstrap order (see design.md D5):
//
//  0. resolve -f, -d from cobra flags
//  1. config.Load(-f)
//
// 2. MkdirAll({d}/logs)            — ensure log dir exists before the logger opens it
// 3. logger.Init(cfg.Logging, {d}/logs)  — writes to the right file from the first line
// 4. stores under {d}/data; skills at {d}/skills
// 5. ApplyLandlock({d}/data, {d}/logs, {d}/skills)
// 6. services, orchestrator, handlers, Gin, ListenAndServe
func serveRun(cmd *cobra.Command, _ []string) error {
	configPath, dataRoot, err := persistentFlags(cmd)
	if err != nil {
		return err
	}

	// 1. Load config. ${VAR} / ${VAR:default} references are expanded by the loader.
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", configPath, err)
	}

	// Derive the four runtime locations from the single -d root (D2/D3/D6): data, logs, skills, tools.
	dataDir := filepath.Join(dataRoot, "data")
	logDir := filepath.Join(dataRoot, "logs")
	skillsDir := filepath.Join(dataRoot, "skills")
	toolsDir := filepath.Join(dataRoot, "tools")

	// 2. Ensure the log directory exists before the logger opens a file in it (D8 fail-fast is enforced inside logger.Init too).
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir %q: %w", logDir, err)
	}

	// 3. Init the zap logger (tee console + file, rotated by lumberjack) and install it as the package default.
	log, err := logger.Init(cfg.Logging, logDir)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("runtime layout",
		zap.String("config", configPath),
		zap.String("data_root", dataRoot),
		zap.String("data_dir", dataDir),
		zap.String("log_dir", logDir),
		zap.String("skills_dir", skillsDir),
		zap.String("tools_dir", toolsDir))

	// 4. MySQL. sqlx.Connect pings on construction so a bad DSN fails fast.
	mysqlStore, err := mysql.New(cfg.MySQL.DSN)
	if err != nil {
		log.Fatal("mysql init failed", zap.Error(err))
	}
	defer func() {
		if cerr := mysqlStore.Close(); cerr != nil {
			log.Warn("mysql close failed", zap.Error(cerr))
		}
	}()

	// Redis. The shared TTL is applied to session-level cache keys.
	redisStore, err := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, RedisCacheTTL)
	if err != nil {
		log.Fatal("redis init failed", zap.Error(err))
	}
	defer func() {
		if cerr := redisStore.Close(); cerr != nil {
			log.Warn("redis close failed", zap.Error(cerr))
		}
	}()

	// FS store for per-user session files, workspace and skills directories. fs.New creates dataDir.
	fsStore, err := fs.New(dataDir)
	if err != nil {
		log.Fatal("fs store init failed", zap.Error(err))
	}

	// Ensure the global skills directory exists (the loader does not create it) so per-subdir landlock below resolves cleanly.
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		log.Fatal("create skills dir failed", zap.Error(err))
	}

	// Ensure the operator tools directory exists (always created, even when empty) so the landlock rule and the in-sandbox --ro-bind always resolve. Operators place CLI binaries here to expose them inside the bash/python/pip sandboxes at $HOME/.local/bin.
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		log.Fatal("create tools dir failed", zap.Error(err))
	}

	// 5. go-landlock (D5/D6). The runtime subdirs the process writes to (data/logs/skills) are restricted read-write — covering logs for lumberjack's post-rotation reopen — while the operator tools dir is restricted read-only, mirroring the in-sandbox --ro-bind as defense-in-depth. Best-effort: a no-op on non-Linux platforms and logged at warn rather than fatal so macOS dev workflows keep running. The application-layer path validation in xizhi still enforces per-user workspace isolation regardless.
	if err := xizhi.ApplyLandlock([]string{dataDir, logDir, skillsDir}, []string{toolsDir}); err != nil {
		log.Warn("landlock not applied; relying on application-layer validation only",
			zap.Error(err))
	}

	// 6. Tool registry. The main registry backs the MCP tools-listing endpoint. Real tool execution during orchestration uses a per-request registry the orchestrator's factory rebuilds scoped to the user's workspace root.
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, dataDir, cfg.Tools.Xizhi)
	webfetch.RegisterAll(reg, cfg.Tools.Webfetch)

	// Sandboxed bash/python/pip execution. Only registered on Linux where bwrap is available; on other platforms enabled tools are ignored. If a tool is explicitly enabled but bwrap is missing on Linux, startup fails fast.
	if cfg.Tools.Executor.Bash.Enabled || cfg.Tools.Executor.Python.Enabled || cfg.Tools.Executor.Pip.Enabled {
		if !executor.IsAvailable() {
			log.Fatal("executor tools enabled but bubblewrap (bwrap) is not available",
				zap.String("platform", runtime.GOOS))
		}
		executorTools := executor.NewTools(cfg.Tools.Executor, func(userID string) string {
			return fsStore.UserWorkspace(userID)
		}, func(userID string) string {
			return fsStore.UserSkills(userID)
		}, skillsDir, toolsDir)
		if err := executor.RegisterAll(reg, executorTools); err != nil {
			log.Fatal("register executor tools failed", zap.Error(err))
		}
	}

	// Skill loader. Discover skills from the global skills directory and per-user data/{userID}/skills/ directories. Register the luban skill tools globally when at least one agent lists them.
	skillLoader := skill.NewLoader(skillsDir, func(userID string) string {
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

	// Keep read_skill registered for backward compatibility when an agent still explicitly references it. New configurations should use luban_read_skill.
	if needsReadSkill(cfg.Agents) {
		if err := skill.RegisterReadSkill(reg, skillLoader); err != nil {
			log.Fatal("register read_skill failed", zap.Error(err))
		}
	}

	// External MCP servers. Connect, discover tools, and register proxy specs into the process-wide registry. Startup fails fast on connection or tool-list errors.
	mcpManager, err := mcpclient.RegisterAllWithManager(context.Background(), reg, cfg.MCP)
	if err != nil {
		log.Fatal("mcp client registration failed", zap.Error(err))
	}
	defer func() {
		if cerr := mcpManager.Close(); cerr != nil {
			log.Warn("mcp client close failed", zap.Error(cerr))
		}
	}()

	// Validate agent MCP tool references against the discovered remote tools.
	serverTools := mcpManager.ServerTools()
	if err := cfg.ValidateAgentMCPTools(toServerToolSet(serverTools)); err != nil {
		log.Fatal("agent mcp tool validation failed", zap.Error(err))
	}

	// Validate agent skill references against global skills. Per-user skills are validated at request time when the userID is known.
	if err := cfg.ValidateAgentSkills("", skillLoader.HasSkill); err != nil {
		log.Fatal("agent skill validation failed", zap.Error(err))
	}

	// 7. Services. SessionService owns the three-layer write path; the message service delegates saves back to SessionService.SaveMessage so writes stay in one place.
	deps := service.SessionDeps{MySQL: mysqlStore, Redis: redisStore, FS: fsStore}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)

	openAIClient := agent.NewOpenAIClient(cfg.OpenAI)
	titleSvc := service.NewTitleService(openAIClient, mysqlStore, cfg.OpenAI)

	// 8. Orchestrator. The workspace-root closure maps the authenticated user id to its workspace directory under the data root; the orchestrator's per-request AgentFactory uses the workspace_root passed to Handle, so the closure here is only a convenience accessor for handlers that need it.
	wsFn := func(userID string) string {
		return fsStore.UserWorkspace(userID)
	}
	orch, err := agent.NewOrchestrator(openAIClient, cfg, reg, serverTools, skillLoader, wsFn)
	if err != nil {
		log.Fatal("orchestrator init failed", zap.Error(err))
	}

	// 9. Handlers. AuthService needs the parsed JWT expire duration; config exposes ParseDuration to handle the short-form suffixes (e.g. "7d").
	jwtExpire, err := cfg.JWT.ParseDuration()
	if err != nil {
		log.Fatal("parse jwt.expire failed", zap.Error(err))
	}
	authSvc := service.NewAuthService(mysqlStore, cfg.JWT.Secret, jwtExpire)
	authHandler := handler.NewAuthHandler(authSvc)
	orchAdapter := handler.NewOrchestratorAdapter(orch)
	sessionHandler := handler.NewSessionHandler(sessSvc, msgSvc, titleSvc, orchAdapter, dataDir)
	workspaceHandler := handler.NewWorkspaceHandler(fsStore, MaxUploadBytes, handler.OnlyOfficeSettings{
		Secret:          cfg.OnlyOffice.Secret,
		ServerURL:       cfg.OnlyOffice.ServerURL,
		InternalBackend: cfg.OnlyOffice.InternalBackend,
	})
	mcpHandler := handler.NewMCPHandler(reg)
	skillHandler := handler.NewSkillHandler(fsStore)

	// 10. Gin engine. Recovery catches panics; trace mints a per-request trace_id and echoes it back on X-Trace-Id; CORS handles preflight.
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.TraceMiddleware())
	engine.Use(middleware.CORS())

	routeDeps := handler.RouteDeps{
		AuthMW:                      middleware.AuthMiddleware(cfg.JWT.Secret),
		QueryTokenAuthMW:            middleware.QueryTokenAuthMiddleware(cfg.JWT.Secret),
		Login:                       authHandler.Login,
		SessionList:                 sessionHandler.ListSessions,
		SessionCreate:               sessionHandler.CreateSession,
		SessionMessages:             sessionHandler.GetSessionMessages,
		SendMessage:                 sessionHandler.SendMessage,
		SessionDelete:               sessionHandler.DeleteSession,
		SessionUpdateTitle:          sessionHandler.UpdateTitle,
		WorkspaceList:               workspaceHandler.List,
		WorkspaceUpload:             workspaceHandler.Upload,
		WorkspaceDownload:           workspaceHandler.Download,
		WorkspaceTokenDownload:      workspaceHandler.TokenDownload,
		WorkspaceContent:            workspaceHandler.Content,
		WorkspaceDelete:             workspaceHandler.Delete,
		WorkspaceRename:             workspaceHandler.Rename,
		WorkspaceOnlyOfficeConfig:   workspaceHandler.OnlyOfficeConfig,
		WorkspaceOnlyOfficeCallback: workspaceHandler.OnlyOfficeCallback,
		MCPTools:                    mcpHandler.Tools,
		SkillsList:                  skillHandler.List,
	}
	handler.RegisterRoutes(engine, routeDeps)

	// 11. HTTP server with graceful shutdown. ListenAndServe runs in a goroutine so the main goroutine can block on the OS signal; on signal we call Shutdown with a 10s grace period and then close the stores (their Close is also deferred above as a backstop for early-return paths).
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
		Handler: engine,
	}
	go func() {
		log.Info("server starting",
			zap.Int("port", cfg.Server.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("shutting down server")

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("server shutdown error", zap.Error(err))
	}
	log.Info("server stopped")
	return nil
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

// needsReadSkill reports whether any agent explicitly lists read_skill in its tools. read_skill is kept as a backward-compatibility entry point; new configurations should use luban_read_skill.
func needsReadSkill(agents config.AgentsConfig) bool {
	for _, cfg := range []config.AgentConfig{agents.Confucius, agents.Chongzhi, agents.Liang} {
		if slices.Contains(cfg.Tools, skill.ToolName) {
			return true
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
