// Package run holds the blowball-api service's serve lifecycle: the api role
// pinned, composed from pkg/boot (shared bootstrap) and the apiserver
// assembly. It mirrors the monolith's api-role path step for step.
package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/handler"
	apiserver "github.com/lush/blowball/services/apiserver"
)

// Serve runs the blowball-api service (the api role pinned): CRUD/history/
// scheduled-task-management routes on server.port, the SchedulerControlService
// on the internal gRPC listener, and the staged-attachment sweeper. It never
// constructs the orchestrator, OpenAI client, tool registry, or MCP manager.
func Serve(configPath, dataRoot string) error {
	rt, err := boot.Setup(configPath, dataRoot, "api")
	if err != nil {
		return err
	}
	log := rt.Log
	defer func() { _ = log.Sync() }()
	defer func() {
		if cerr := rt.MySQLStore.Close(); cerr != nil {
			log.Warn("mysql close failed", zap.Error(cerr))
		}
	}()
	defer func() {
		if cerr := rt.RedisStore.Close(); cerr != nil {
			log.Warn("redis close failed", zap.Error(cerr))
		}
	}()
	if rt.SchedulerConn != nil {
		defer func() { _ = rt.SchedulerConn.Close() }()
	}

	sessSvc := boot.NewSessionService(rt)

	engine := boot.NewEngine()
	handler.RegisterHealthz(engine)

	// Internal gRPC: the api partition serves the SchedulerControlService.
	var internalSrv *grpc.Server
	var internalLis net.Listener
	if rt.Cfg.InternalRPC.Enabled {
		internalSrv, internalLis, err = boot.NewInternalRPCServer(rt.Cfg.InternalRPC)
		if err != nil {
			return err
		}
		apiserver.RegisterControlService(internalSrv, rt)
		boot.LogInternalRPCSecurity(log, rt.Cfg.InternalRPC)
	}

	handler.RegisterAPIRoutes(engine, apiserver.Wire(rt, sessSvc))

	if internalSrv != nil {
		go func() {
			if serr := internalSrv.Serve(internalLis); serr != nil && !errors.Is(serr, grpc.ErrServerStopped) {
				log.Fatal("internal rpc server failed", zap.Error(serr))
			}
		}()
		log.Info("internal rpc listening",
			zap.String("role", "api"),
			zap.String("addr", rt.Cfg.InternalRPC.Listen))
	}

	stopSweep := apiserver.StartAttachmentSweeper(rt)
	defer stopSweep()
	log.Info("staged attachment sweeper started", zap.String("role", "api"))

	port := rt.Cfg.Server.Port
	log.Info("server starting",
		zap.String("role", "api"),
		zap.Int("port", port),
		zap.Strings("route_groups", boot.RouteGroups("api")))

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
	log.Info("shutting down server", zap.String("role", "api"))

	ctx, cancel := context.WithTimeout(context.Background(), boot.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("server shutdown error", zap.Error(err))
	}
	if internalSrv != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), boot.ShutdownTimeout)
		stopped := make(chan struct{})
		go func() {
			internalSrv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-stopCtx.Done():
			internalSrv.Stop()
		}
		stopCancel()
	}
	log.Info("server stopped", zap.String("role", "api"))
	return nil
}
