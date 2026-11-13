// Package boot holds the process bootstrap shared by every blowball server
// binary: the development monolith (cmd/blowball) and the per-role service
// binaries (services/apiserver/cmd/blowball-api, services/agent/cmd/blowball-agent).
//
// Setup performs the role-agnostic startup — config, runtime directories,
// role-aware logger, MySQL/Redis/FS stores, market clients, scheduler gRPC
// client, resource services, shared-storage health check, and landlock — and
// returns the Runtime bundle the role assemblies consume. The role assemblies
// themselves live in services/apiserver and services/agent; this package must
// not depend on them.
package boot

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/artifact"
	"github.com/lush/blowball/pkg/config"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/mcpmarket"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/modelselect"
	"github.com/lush/blowball/pkg/resource"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/pkg/scheduledcontrol"
	"github.com/lush/blowball/pkg/service"
	"github.com/lush/blowball/pkg/skillmarket"
	"github.com/lush/blowball/pkg/storage"
	"github.com/lush/blowball/pkg/store/fs"
	"github.com/lush/blowball/pkg/store/mysql"
	"github.com/lush/blowball/pkg/store/redis"
	"github.com/lush/blowball/pkg/tool/executor"
	"github.com/lush/blowball/pkg/tool/xizhi"
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

// InternalRPCOptions derives the authenticated-gRPC server/dial option pair
// from the internal_rpc config block. Both are zero when the link is off.
func InternalRPCOptions(cfg config.InternalRPCConfig) (rpcx.ServerOptions, rpcx.DialOptions) {
	if !cfg.Enabled {
		return rpcx.ServerOptions{}, rpcx.DialOptions{}
	}
	return rpcx.ServerOptions{
		SecurityMode: cfg.SecurityMode, TLS: cfg.TLS.ToRPCX(), PeerTokens: cfg.PeerTokens, RequireDeadline: true,
	}, rpcx.DialOptions{
		Target: cfg.SchedulerEndpoint, SecurityMode: cfg.SecurityMode, TLS: cfg.SchedulerTLS.ToRPCX(),
		ClientToken: cfg.ClientToken, Timeout: cfg.Timeout,
	}
}

// LogInternalRPCSecurity warns once when the internal RPC link runs in the
// loopback-only plaintext-token mode (never for mTLS or a disabled link), and
// never includes credential values.
func LogInternalRPCSecurity(log *zap.Logger, cfg config.InternalRPCConfig) {
	if cfg.Enabled && cfg.SecurityMode == rpcx.SecurityModePlaintextToken {
		log.Warn("internal rpc plaintext mode enabled: traffic is unencrypted and server identity is not verified",
			zap.String("security_mode", string(cfg.SecurityMode)), zap.String("listen", cfg.Listen),
			zap.String("scheduler_endpoint", cfg.SchedulerEndpoint))
	}
}

// OpenAIKeyRequired reports whether the role needs a configured openai.api_key
// at startup. The api role never drives the LLM, so it does not require one;
// the all and agent roles construct the orchestrator and title service, which do.
func OpenAIKeyRequired(role string) bool {
	return role != "api"
}

// RouteGroups returns the human-readable names of the route partitions a role
// registers, for the startup log line.
func RouteGroups(role string) []string {
	switch role {
	case "api":
		return []string{"api"}
	case "agent":
		return []string{"agent"}
	default:
		return []string{"api", "agent"}
	}
}

// ExecutorConfigured reports whether the sandboxed bash executor tool is
// enabled in config. It gates both executor registration in the agent assembly
// and the shared-mode bwrap self-check in Setup. (The dedicated python/pip_install
// executors were removed; Python and pip run via bash.)
func ExecutorConfigured(cfg *config.Config) bool {
	return cfg.Tools.Executor.Bash.Enabled
}

// Runtime bundles everything the shared setup produces and both role
// assemblies consume: the parsed config, the selected role, the derived
// runtime directories, the zap logger, the three shared stores, the market
// clients, and the standalone-scheduler integration services. Stores are
// concrete because they are constructed once at startup and closed via
// deferred Close in the serve lifecycle.
type Runtime struct {
	Cfg          *config.Config
	Role         string
	DataDir      string
	LogDir       string
	SkillsDir    string
	ToolsDir     string
	MarketDir    string
	MCPMarketDir string
	Log          *zap.Logger
	MySQLStore   *mysql.Store
	RedisStore   *redis.Store
	FSStore      *fs.Store
	// Market is the skill-market client (skill-market capability), nil when
	// the skill_market config block is off. Constructed here in the shared
	// setup because BOTH roles consume it: the api role for the skills list
	// endpoint, the agent role for the luban tools and the bash sandbox's
	// per-skill market mounts.
	Market *skillmarket.Client
	// MCPMarket is the MCP-market client (mcp-market capability), nil when
	// the mcp_market config block is off. Both roles consume it: the api role
	// for the MCP tools endpoint merge, the agent role for the mcp_* tools,
	// the prompt advertisement, and manager connections.
	MCPMarket *mcpmarket.Client
	// Standalone-scheduler integration (extract-scheduler-service). The
	// client is nil when internal_rpc is disabled; the resource guard then
	// fails closed for managed-path writes while ordinary workspace behavior
	// is unchanged.
	SchedulerConn     *grpc.ClientConn
	SchedulerClient   schedulerv1.SchedulerServiceClient
	ResourceSvc       *resource.Service
	ResourceGuard     *resource.Guard
	ScheduledSessions *service.ScheduledSessions
	ControlSvc        *scheduledcontrol.Service
}

// Setup performs the shared bootstrap that runs for every role: load
// config, derive the six runtime directories, init the role-aware logger,
// enforce the role-aware openai.api_key requirement, connect MySQL/Redis, open
// the FS store, ensure the skills/tools/market dirs exist, and apply Landlock.
// It matches the pre-split bootstrap step-for-step; the only additions are the
// role-scoped log filename and the relaxed openai-key check for the api role.
//
// Store-init failures use log.Fatal (as before) so a bad DSN or unreachable
// Redis aborts startup with a clear message rather than a generic CLI error.
func Setup(configPath, dataRoot, role string) (*Runtime, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", configPath, err)
	}

	// Derive the six runtime locations from the single -d root (D2/D3/D6): data, logs, skills, tools, skills-market, mcp-market.
	dataDir := filepath.Join(dataRoot, "data")
	logDir := filepath.Join(dataRoot, "logs")
	skillsDir := filepath.Join(dataRoot, "skills")
	toolsDir := filepath.Join(dataRoot, "tools")
	marketDir := filepath.Join(dataRoot, "skills-market")
	mcpMarketDir := filepath.Join(dataRoot, "mcp-market")

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
		if OpenAIKeyRequired(role) {
			log.Fatal("openai.api_key must be non-empty for this role",
				zap.String("role", role))
		}
		log.Warn("openai.api_key is empty; allowed because this role never calls the LLM",
			zap.String("role", role))
	}

	rt := &Runtime{
		Cfg:          cfg,
		Role:         role,
		DataDir:      dataDir,
		LogDir:       logDir,
		SkillsDir:    skillsDir,
		ToolsDir:     toolsDir,
		MarketDir:    marketDir,
		MCPMarketDir: mcpMarketDir,
		Log:          log,
	}

	// Skill-market client (skill-market capability): nil for a disabled
	// config, constructed here in the shared setup because both roles consume
	// it. Pure HTTP client holder — no store dependencies, no startup probe
	// (failures degrade per call, never gate boot).
	rt.Market = skillmarket.New(cfg.SkillMarket, marketDir)
	// MCP-market client (mcp-market capability): the same shared-setup shape.
	rt.MCPMarket = mcpmarket.New(cfg.MCPMarket, mcpMarketDir)

	log.Info("runtime layout",
		zap.String("role", role),
		zap.String("config", configPath),
		zap.String("data_root", dataRoot),
		zap.String("data_dir", dataDir),
		zap.String("log_dir", logDir),
		zap.String("skills_dir", skillsDir),
		zap.String("tools_dir", toolsDir),
		zap.String("skills_market_dir", marketDir),
		zap.Bool("skill_market_enabled", rt.Market.Enabled()))
	log.Info("mcp market layout",
		zap.String("mcp_market_dir", mcpMarketDir),
		zap.Bool("mcp_market_enabled", rt.MCPMarket.Enabled()))

	// MySQL. sqlx.Connect pings on construction so a bad DSN fails fast.
	mysqlStore, err := mysql.New(cfg.MySQL.DSN)
	if err != nil {
		log.Fatal("mysql init failed", zap.Error(err))
	}
	rt.MySQLStore = mysqlStore

	// Redis. The shared TTL is applied to session-level cache keys.
	redisStore, err := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, RedisCacheTTL)
	if err != nil {
		log.Fatal("redis init failed", zap.Error(err))
	}
	rt.RedisStore = redisStore

	// FS store for per-user session files, workspace and skills directories. fs.New creates dataDir.
	fsStore, err := fs.New(dataDir)
	if err != nil {
		log.Fatal("fs store init failed", zap.Error(err))
	}
	rt.FSStore = fsStore

	// Standalone Scheduler client (extract-scheduler-service). A Scheduler that
	// is temporarily offline must not block normal startup: the gRPC client is
	// lazy and per-call failures surface as UNAVAILABLE (HTTP 503) with no
	// local fallback. A TLS material/config error is fatal.
	if cfg.InternalRPC.Enabled {
		_, clientOpts := InternalRPCOptions(cfg.InternalRPC)
		conn, derr := rpcx.Dial(clientOpts)
		if derr != nil {
			log.Fatal("scheduler rpc client init failed", zap.Error(derr))
		}
		rt.SchedulerConn = conn
		rt.SchedulerClient = schedulerv1.NewSchedulerServiceClient(conn)
	}
	var owner resource.TaskOwner = unavailableTaskOwner{}
	if rt.SchedulerClient != nil {
		owner = resource.NewSchedulerTaskOwner(rt.SchedulerClient)
	}
	resLimits := resource.Limits{
		MaxFiles:        cfg.ScheduledResources.MaxFiles,
		MaxTotalBytes:   cfg.ScheduledResources.MaxTotalBytes,
		MaxFileBytes:    cfg.ScheduledResources.MaxFileBytes,
		MaxContextFiles: cfg.ScheduledResources.MaxContextFiles,
		MaxContextBytes: cfg.ScheduledResources.MaxContextBytes,
	}
	rt.ResourceSvc = resource.New(rt.MySQLStore, owner, fsStore.UserWorkspace, resLimits)
	rt.ResourceGuard = resource.NewGuard(rt.ResourceSvc)
	rt.ControlSvc = scheduledcontrol.New(rt.MySQLStore, rt.ResourceSvc, modelselect.NewConfig(cfg))
	rt.ScheduledSessions = service.NewScheduledSessions(rt.MySQLStore.ForSource(model.SessionSourceScheduled), rt.RedisStore).
		WithDeletionGuard(func(ctx context.Context, sessionID string) (string, error) {
			rec, gerr := rt.MySQLStore.GetScheduledExecutionBySession(ctx, sessionID)
			if gerr != nil {
				return "", gerr
			}
			// Any non-terminal execution fact means a stop proof is still
			// missing (accepted/preparing may start any moment; start_intent/
			// running/uncertain may be side-effecting).
			if rec != nil && !rec.Terminal() {
				return rec.Status + ":" + rec.Reason, nil
			}
			return "", nil
		})

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
		if ExecutorConfigured(cfg) && role != "api" {
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
	if err := os.MkdirAll(mcpMarketDir, 0o755); err != nil {
		log.Fatal("create mcp-market dir failed", zap.Error(err))
	}

	// go-landlock (D5/D6). The runtime subdirs the process writes to (data/logs/skills) are restricted read-write — covering logs for lumberjack's post-rotation reopen — plus operator extra_read_write; the operator tools dir and the skills-market dir are restricted read-only (the agent never writes market payloads — operators sync them), plus operator extra_read_only; the configurable system_read_only baseline is restricted read-only too. Best-effort: a no-op on non-Linux platforms and logged at warn rather than fatal so macOS dev workflows keep running. The application-layer path validation in xizhi still enforces per-user workspace isolation regardless. landlock.enabled: false skips ApplyLandlock entirely (warning-only). All defaults reproduce the pre-configurability literals.
	rwDirs := append([]string{dataDir, logDir, skillsDir}, cfg.Landlock.ExtraReadWrite...)
	roDirs := append([]string{toolsDir, marketDir, mcpMarketDir}, cfg.Landlock.ExtraReadOnly...)
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

// NewEngine builds a gin.Engine with the standard middleware chain shared by
// every role: Recovery (panic safety) → Trace (per-request trace_id) → CORS.
// Auth is applied per-route-group inside RegisterAPIRoutes / RegisterAgentRoutes
// so it stays the final middleware before the handler.
func NewEngine() *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(middleware.TraceMiddleware())
	engine.Use(middleware.CORS())
	return engine
}

// NewSessionService builds the shared (store-only) SessionService both roles
// need. Store-only, so it carries no agent-layer dependency; both the api CRUD
// handlers and the agent streaming handler use it. (The write-behind drain and
// flusher hooks are retired with the ingest queue — refactor-message-storage:
// persistence is a direct transactional MySQL write.)
func NewSessionService(rt *Runtime) *service.SessionService {
	sessDeps := service.SessionDeps{
		MySQL: rt.MySQLStore,
		Redis: rt.RedisStore,
		FS:    rt.FSStore,
	}
	return service.NewSessionService(sessDeps).
		WithAttachmentDir(rt.FSStore.UserAttachmentDir).
		WithQueueClearer(rt.RedisStore.RunStore().ClearQueue)
}

// NewArtifactService builds the turn-artifacts service: office-vers
// (onlyoffice.version_service_url) as the version store plus the MySQL
// version index. An empty version_service_url leaves the store nil —
// artifacts are then announced without version pins (degraded but live).
func NewArtifactService(rt *Runtime) *artifact.Service {
	var store artifact.VersionStore
	if u := rt.Cfg.OnlyOffice.VersionServiceURL; u != "" {
		store = artifact.NewOfficeVersStore(u)
	}
	return artifact.NewService(store, rt.MySQLStore, rt.Cfg.Artifact.MaxSnapshotBytes)
}

// NewInternalRPCServer builds the authenticated internal gRPC server with the
// standard health service registered and starts listening on
// internal_rpc.listen. Role-specific services mount on the returned server:
// the api partition registers the SchedulerControlService, the agent
// partition the ScheduledRuntimeService (see the two service assemblies).
func NewInternalRPCServer(cfg config.InternalRPCConfig) (*grpc.Server, net.Listener, error) {
	serverOpts, _ := InternalRPCOptions(cfg)
	srv, err := rpcx.NewServer(serverOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("internal rpc server security: %w", err)
	}
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("internal rpc listen %s: %w", cfg.Listen, err)
	}
	return srv, lis, nil
}

// unavailableTaskOwner fails closed when no Scheduler is configured: managed
// `.cron` writes cannot be authorized without the authoritative task owner.
type unavailableTaskOwner struct{}

func (unavailableTaskOwner) OwnsTask(context.Context, string, string) (bool, error) {
	return false, fmt.Errorf("scheduler is not configured")
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
