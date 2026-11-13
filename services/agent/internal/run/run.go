// Package run holds the blowball-agent service's serve lifecycle: the agent
// role pinned, composed from pkg/boot (shared bootstrap) and the agentserver
// assembly. It mirrors the monolith's agent-role path step for step.
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
	agentserver "github.com/lush/blowball/services/agent"
)

// Serve runs the blowball-agent service (the agent role pinned): the
// streaming message endpoint, turn lifecycle, MCP tool list, and the
// ScheduledRuntimeService on the internal gRPC listener, all on
// server.agent_port. Shutdown ordering matches the monolith exactly:
// admission gate → HTTP drain → internal RPC stop → Assembly.Finalize.
func Serve(configPath, dataRoot string) error {
	rt, err := boot.Setup(configPath, dataRoot, "agent")
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

	// Internal gRPC: the agent partition serves the ScheduledRuntimeService
	// (registered by Wire when the scheduler link is configured).
	var internalSrv *grpc.Server
	var internalLis net.Listener
	if rt.Cfg.InternalRPC.Enabled {
		internalSrv, internalLis, err = boot.NewInternalRPCServer(rt.Cfg.InternalRPC)
		if err != nil {
			return err
		}
		boot.LogInternalRPCSecurity(log, rt.Cfg.InternalRPC)
	}

	asm := agentserver.Wire(rt, sessSvc, internalSrv)
	defer asm.Close(log)
	handler.RegisterAgentRoutes(engine, asm.Routes)

	if internalSrv != nil {
		go func() {
			if serr := internalSrv.Serve(internalLis); serr != nil && !errors.Is(serr, grpc.ErrServerStopped) {
				log.Fatal("internal rpc server failed", zap.Error(serr))
			}
		}()
		log.Info("internal rpc listening",
			zap.String("role", "agent"),
			zap.String("addr", rt.Cfg.InternalRPC.Listen))
	}

	port := rt.Cfg.Server.AgentPort
	log.Info("server starting",
		zap.String("role", "agent"),
		zap.Int("port", port),
		zap.Strings("route_groups", boot.RouteGroups("agent")))

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
	log.Info("shutting down server", zap.String("role", "agent"))

	// Gate new turns (and park the reconciler) before anything else drains,
	// so the shutdown-bound turn cancels never pump the queued heads —
	// entries survive in Redis for the post-restart reconciler.
	asm.StopAdmission()
	log.Info("agent turn admission and scheduler stopped", zap.String("role", "agent"))

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
	log.Info("server stopped", zap.String("role", "agent"))

	// Ordered drain: cancel/wait running turns, drain the raw-capture
	// buffer, release the memory client.
	asm.Finalize(log)
	return nil
}
