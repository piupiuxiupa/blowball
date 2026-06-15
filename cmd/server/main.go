// Package main is the blowball HTTP server entry point.
//
// main wires every internal package — config, logger, stores, services, agent
// orchestrator, tool registry, HTTP handlers and middleware — and runs the Gin
// server with graceful shutdown on SIGINT/SIGTERM.
//
// The bootstrap order matters:
//  1. config.yaml (the loader expands ${VAR} references).
//  2. zap logger (so every step below logs through it).
//  3. MySQL (sqlx connection pool), Redis (go-redis client), FS store.
//  4. go-landlock restriction on the data directory (best-effort; no-op on
//     non-Linux platforms such as the macOS dev environment).
//  5. Tool registry with the Xizhi file tools registered against a placeholder
//     root. The orchestrator's per-request AgentFactory rebuilds a registry
//     scoped to the requesting user's workspace, so the placeholder root here
//     only backs the MCP tools-listing endpoint.
//  6. Services (auth, session, message, title), the OpenAI client, the agent
//     orchestrator, and the HTTP handlers.
//  7. Gin engine with recovery + trace + CORS middleware, route registration,
//     then ListenAndServe in a goroutine so the main goroutine can block on
//     the OS signal that triggers Shutdown.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
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
	"github.com/lush/blowball/internal/tool/mcpclient"
	"github.com/lush/blowball/internal/tool/webfetch"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// DataDir is the on-disk root for per-user workspaces, session files and
// skills. Landlock restricts the process to it (on Linux) and every store that
// touches disk is rooted here.
const DataDir = "data"

// RedisCacheTTL is the expiration applied to every session-level cache write.
// The spec defaults to 24h; if the deployment wants a different value it can be
// surfaced through config later without touching this constant.
const RedisCacheTTL = 24 * time.Hour

// MaxUploadBytes caps a single multipart upload at 50 MiB. Larger uploads are
// rejected with 413 before they reach disk.
const MaxUploadBytes = 50 << 20

// ShutdownTimeout is the upper bound on draining in-flight requests after a
// SIGINT/SIGTERM, per the api-server spec's graceful-shutdown requirement.
const ShutdownTimeout = 10 * time.Second

func main() {
	// 1. Load config. ${VAR} / ${VAR:default} references are expanded by the
	// loader, so callers can drive every secret / DSN through the environment.
	cfg, err := config.Load("config.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	// 2. Init the zap logger and install it as the package default so any
	// caller of logger.L() (services, stores, middleware) picks it up.
	log, err := logger.Init(cfg.Logging.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	// 3. MySQL. sqlx.Connect pings on construction so a bad DSN fails fast.
	mysqlStore, err := mysql.New(cfg.MySQL.DSN)
	if err != nil {
		log.Fatal("mysql init failed", zap.Error(err))
	}
	defer func() {
		if cerr := mysqlStore.Close(); cerr != nil {
			log.Warn("mysql close failed", zap.Error(cerr))
		}
	}()

	// 4. Redis. The shared TTL is applied to session-level cache keys.
	redisStore, err := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, RedisCacheTTL)
	if err != nil {
		log.Fatal("redis init failed", zap.Error(err))
	}
	defer func() {
		if cerr := redisStore.Close(); cerr != nil {
			log.Warn("redis close failed", zap.Error(cerr))
		}
	}()

	// 5. FS store for per-user session files, workspace and skills directories.
	fsStore, err := fs.New(DataDir)
	if err != nil {
		log.Fatal("fs store init failed", zap.Error(err))
	}

	// 6. go-landlock. Best-effort: a no-op on non-Linux platforms and logged
	// at warn rather than fatal so macOS dev workflows keep running. The
	// application-layer path validation in xizhi still enforces per-user
	// workspace isolation regardless.
	if err := xizhi.ApplyLandlock(DataDir); err != nil {
		log.Warn("landlock not applied; relying on application-layer validation only",
			zap.Error(err))
	}

	// 7. Tool registry. The main registry backs the MCP tools-listing endpoint.
	// Real tool execution during orchestration uses a per-request registry the
	// orchestrator's factory rebuilds scoped to the user's workspace root.
	reg := tool.NewRegistry()
	xizhi.RegisterAll(reg, DataDir, cfg.Tools.Xizhi)
	webfetch.RegisterAll(reg, cfg.Tools.Webfetch)

	// 7a. External MCP servers. Connect, discover tools, and register proxy
	// specs into the process-wide registry. Startup fails fast on connection or
	// tool-list errors so misconfiguration is surfaced immediately.
	mcpClose, err := mcpclient.RegisterAll(context.Background(), reg, cfg.MCP)
	if err != nil {
		log.Fatal("mcp client registration failed", zap.Error(err))
	}
	defer func() {
		if cerr := mcpClose(); cerr != nil {
			log.Warn("mcp client close failed", zap.Error(cerr))
		}
	}()

	// 8. Services. SessionService owns the three-layer write path; the message
	// service delegates saves back to SessionService.SaveMessage so writes stay
	// in one place.
	deps := service.SessionDeps{MySQL: mysqlStore, Redis: redisStore, FS: fsStore}
	sessSvc := service.NewSessionService(deps)
	msgSvc := service.NewMessageService(deps, sessSvc.SaveMessage)

	openAIClient := agent.NewOpenAIClient(cfg.OpenAI)
	titleSvc := service.NewTitleService(openAIClient, mysqlStore, cfg.OpenAI)

	// 9. Orchestrator. The workspace-root closure maps the authenticated user
	// id to its workspace directory under the data root; the orchestrator's
	// per-request AgentFactory uses the workspace_root passed to Handle, so the
	// closure here is only a convenience accessor for handlers that need it.
	wsFn := func(userID string) string {
		return fsStore.UserWorkspace(userID)
	}
	orch, err := agent.NewOrchestrator(openAIClient, cfg, reg, wsFn)
	if err != nil {
		log.Fatal("orchestrator init failed", zap.Error(err))
	}

	// 10. Handlers. AuthService needs the parsed JWT expire duration; config
	// exposes ParseDuration to handle the short-form suffixes (e.g. "7d").
	jwtExpire, err := cfg.JWT.ParseDuration()
	if err != nil {
		log.Fatal("parse jwt.expire failed", zap.Error(err))
	}
	authSvc := service.NewAuthService(mysqlStore, cfg.JWT.Secret, jwtExpire)
	authHandler := handler.NewAuthHandler(authSvc)
	orchAdapter := handler.NewOrchestratorAdapter(orch)
	sessionHandler := handler.NewSessionHandler(sessSvc, msgSvc, titleSvc, orchAdapter, DataDir)
	workspaceHandler := handler.NewWorkspaceHandler(fsStore, MaxUploadBytes)
	mcpHandler := handler.NewMCPHandler(reg)
	skillHandler := handler.NewSkillHandler(fsStore)

	// 11. Gin engine. Recovery catches panics; trace mints a per-request
	// trace_id and echoes it back on X-Trace-Id; CORS handles preflight.
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.TraceMiddleware())
	engine.Use(middleware.CORS())

	routeDeps := handler.RouteDeps{
		AuthMW:            middleware.AuthMiddleware(cfg.JWT.Secret),
		Login:             authHandler.Login,
		SessionList:       sessionHandler.ListSessions,
		SessionCreate:     sessionHandler.CreateSession,
		SessionMessages:   sessionHandler.GetSessionMessages,
		SendMessage:       sessionHandler.SendMessage,
		WorkspaceList:     workspaceHandler.List,
		WorkspaceUpload:   workspaceHandler.Upload,
		WorkspaceDownload: workspaceHandler.Download,
		WorkspaceContent:  workspaceHandler.Content,
		MCPTools:          mcpHandler.Tools,
		SkillsList:        skillHandler.List,
	}
	handler.RegisterRoutes(engine, routeDeps)

	// 12. HTTP server with graceful shutdown. ListenAndServe runs in a
	// goroutine so the main goroutine can block on the OS signal; on signal we
	// call Shutdown with a 10s grace period and then close the stores (their
	// Close is also deferred above as a backstop for early-return paths).
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
		Handler: engine,
	}
	go func() {
		log.Info("server starting",
			zap.Int("port", cfg.Server.Port),
			zap.String("data_dir", DataDir))
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
}
