// Package apiserver assembles the api-role route partition: auth, session
// CRUD, message-history read, workspace file CRUD, scheduled-task management,
// the skills/model/llm-token endpoints, and the staged-attachment sweeper.
//
// It is consumed by the development monolith (cmd/blowball, --role all|api)
// and the blowball-api service binary. Fault isolation: this assembly never
// constructs the orchestrator, OpenAI client, tool registry, or MCP manager.
package apiserver

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/handler"
	"github.com/lush/blowball/pkg/middleware"
	"github.com/lush/blowball/pkg/model"
	"github.com/lush/blowball/pkg/run"
	"github.com/lush/blowball/pkg/scheduledcontrol"
	"github.com/lush/blowball/pkg/service"
)

// Wire builds the CRUD services/handlers for the api role (and contributes
// them in the all role): auth, session CRUD, message-history read, manual title
// update, workspace file CRUD, and the skills list. It returns a RouteDeps
// populated with only the API-route handlers plus the auth middleware; the
// agent-partition fields (SendMessage, MCPTools) are left nil.
//
// The api role's TitleService is built with a nil LLM client — SetManualTitle
// never calls the LLM, so the api role needs no OpenAI dependency.
func Wire(rt *boot.Runtime, sessSvc *service.SessionService) handler.RouteDeps {
	cfg := rt.Cfg

	jwtExpire, err := cfg.JWT.ParseDuration()
	if err != nil {
		rt.Log.Fatal("parse jwt.expire failed", zap.Error(err))
	}
	authSvc := service.NewAuthService(rt.MySQLStore, cfg.JWT.Secret, jwtExpire, cfg.Auth.IsPasswordRequired())
	authHandler := handler.NewAuthHandler(authSvc)

	// The api role never calls the LLM; a nil client is safe because
	// SetManualTitle only touches MySQL (see TitleService.SetManualTitle).
	// The title config carries the wiring-resolved title model (title_model
	// or the default catalog entry) — see TitleModelName.
	titleCfg := cfg.OpenAI
	titleCfg.TitleModel = cfg.TitleModelName()
	titleSvc := service.NewTitleService(nil, rt.MySQLStore, titleCfg)
	// The run store feeds the session list's generating flag; the api role
	// reads the shared Redis claims but never constructs a run registry.
	sessionHandler := handler.NewSessionHandler(sessSvc, titleSvc, rt.RedisStore.RunStore())
	// Session message queue (session-message-queue): the queue read endpoint
	// and the additive queued_count markers ride the same Store-only view —
	// both stay off (byte-for-byte responses) when the queue is disabled.
	queueHandler := handler.NewQueueHandler(sessSvc, run.NewManager(rt.RedisStore.RunStore(), nil))
	if cfg.Messages.Queue.Enabled {
		sessionHandler.EnableQueueInfo()
	}
	subAgentService := service.NewSubAgentService(rt.MySQLStore)
	subAgentHandler := handler.NewSubAgentHandler(subAgentService)
	workspaceHandler := handler.NewWorkspaceHandler(rt.FSStore, boot.MaxUploadBytes, handler.OnlyOfficeSettings{
		Secret:            cfg.OnlyOffice.Secret,
		ServerURL:         cfg.OnlyOffice.ServerURL,
		InternalBackend:   cfg.OnlyOffice.InternalBackend,
		VersionServiceURL: cfg.OnlyOffice.VersionServiceURL,
	}).WithResourceGuard(rt.ResourceGuard)
	skillHandler := handler.NewSkillHandler(rt.FSStore, rt.Market)
	modelListHandler := handler.NewModelListHandler(cfg.ModelCatalog(), cfg.DefaultModelName(), cfg.OpenAI.DefaultReasoningEffort)
	llmTokenHandler := handler.NewLLMTokenHandler(service.NewLLMTokenService(rt.MySQLStore))
	var taskRPC handler.ScheduledTaskRPC = scheduledcontrol.UnavailableScheduledTaskRPC{}
	var occurrenceLookup handler.ScheduledOccurrenceLookup
	if rt.SchedulerClient != nil {
		adapter := scheduledcontrol.NewSchedulerManagementClient(rt.SchedulerClient)
		taskRPC = adapter
		occurrenceLookup = adapter
	}
	scheduledTaskHandler := handler.NewScheduledTaskHandler(taskRPC, cfg.DefaultModelName()).
		WithContentExistence(func(ctx context.Context, source, sessionID string) (bool, error) {
			return rt.MySQLStore.SessionContentExists(ctx, model.SessionSource(source), sessionID)
		})
	scheduledSessionHandler := handler.NewScheduledSessionHandler(rt.ScheduledSessions, occurrenceLookup, cfg.DefaultModelName())
	resourceHandler := handler.NewScheduledResourceHandler(rt.ResourceSvc)
	artifactHandler := handler.NewArtifactHandler(boot.NewArtifactService(rt))
	attachmentHandler := handler.NewAttachmentHandler(rt.MySQLStore, sessSvc, rt.FSStore)

	return handler.RouteDeps{
		AuthMW:                           middleware.AuthMiddleware(cfg.JWT.Secret),
		QueryTokenAuthMW:                 middleware.QueryTokenAuthMiddleware(cfg.JWT.Secret),
		Login:                            authHandler.Login,
		SessionList:                      sessionHandler.ListSessions,
		SessionCreate:                    sessionHandler.CreateSession,
		SessionGet:                       sessionHandler.GetSession,
		SessionQueue:                     queueHandler.ListQueue,
		SessionMessages:                  sessionHandler.GetSessionMessages,
		AttachmentUpload:                 attachmentHandler.Upload,
		AttachmentStagedUpload:           attachmentHandler.UploadStaged,
		AttachmentContent:                attachmentHandler.Content,
		SubAgentRuns:                     subAgentHandler.ListRuns,
		SubAgentRunDetail:                subAgentHandler.GetRun,
		SessionDelete:                    sessionHandler.DeleteSession,
		ScheduledTaskCreate:              scheduledTaskHandler.Create,
		ScheduledTaskList:                scheduledTaskHandler.List,
		ScheduledTaskGet:                 scheduledTaskHandler.Get,
		ScheduledTaskUpdate:              scheduledTaskHandler.Update,
		ScheduledTaskDelete:              scheduledTaskHandler.Delete,
		ScheduledTaskRuns:                scheduledTaskHandler.Runs,
		ScheduledResourceGet:             resourceHandler.Get,
		ScheduledResourcePublish:         resourceHandler.Publish,
		ScheduledSessionList:             scheduledSessionHandler.List,
		ScheduledSessionGet:              scheduledSessionHandler.Get,
		ScheduledSessionMessages:         scheduledSessionHandler.Messages,
		ScheduledSessionDelete:           scheduledSessionHandler.Delete,
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
		ArtifactResolve:                  artifactHandler.Resolve,
		ArtifactVersionContent:           artifactHandler.VersionContent,
		SkillsList:                       skillHandler.List,
		ModelsList:                       modelListHandler.List,
		LLMTokenGet:                      llmTokenHandler.Get,
		LLMTokenPut:                      llmTokenHandler.Put,
		LLMTokenDelete:                   llmTokenHandler.Delete,
	}
}

// RegisterControlService mounts the SchedulerControlService on the internal
// gRPC server. The api partition serves it; the agent partition serves the
// ScheduledRuntimeService instead (see the agent assembly). The Scheduler
// calls these with its own service identity.
func RegisterControlService(srv *grpc.Server, rt *boot.Runtime) {
	schedulerv1.RegisterSchedulerControlServiceServer(srv, rt.ControlSvc)
}

// StartAttachmentSweeper runs the staged-attachment TTL reaper
// (add-staged-attachment-upload): staged uploads never get a session-delete
// cleanup, so roles serving the API partition run exactly one sweeper. The
// returned stop function cancels the loop; best-effort — the sweeper logs and
// survives its own failures.
func StartAttachmentSweeper(rt *boot.Runtime) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go service.NewAttachmentSweeper(rt.MySQLStore, rt.FSStore.UserAttachmentStagingDir).Start(ctx)
	return cancel
}
