// Package main is the blowball unified CLI entry point.
//
// The binary exposes `serve` and `seed` subcommands with shared `-f`/`--config` and
// `-d`/`--data-dir` flags. See main.go for the command wiring; this file holds the
// `serve` subcommand — a thin composition over pkg/boot (shared bootstrap) and the
// per-role assemblies in services/apiserver and services/agent. seed.go holds the
// `seed` subcommand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/handler"
	agentserver "github.com/lush/blowball/services/agent"
	apiserver "github.com/lush/blowball/services/apiserver"
)

// validRoles is the set of accepted --role values. "all" is the default and
// preserves the pre-split single-process behavior; "api" and "agent" select
// the partitioned process roles (see the service-roles spec).
var validRoles = []string{"all", "api", "agent"}

// serveCmd parses the serve flags and runs the server bootstrap. --role
// selects which route partition this process serves. "all" (default) is the
// rollback path: one process, full route set, single listener on server.port
// — identical to the pre-split monolith.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	role := fs.String("role", "all", "process role: all|api|agent")
	configPath, dataRoot := sharedFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Validate --role before any setup so a bad value exits non-zero without
	// touching the filesystem or opening connections.
	if err := validateRole(*role); err != nil {
		return err
	}
	return serve(*configPath, *dataRoot, *role)
}

// serve is the monolith bootstrap, composing the shared setup with the two
// role assemblies:
//
//  1. shared setup (boot.Setup): config → runtime dirs → logger (role-aware
//     filename) → MySQL/Redis/FS → skills/tools dirs → Landlock. Plus the
//     role-aware openai.api_key requirement.
//  2. build the shared (store-only) SessionService both roles need
//  3. build the engine (Recovery → Trace → CORS) and mount /healthz
//  4. register routes by role: apiserver.Wire (CRUD) and/or agentserver.Wire
//     (streaming + MCP)
//  5. per-role HTTP listener + graceful shutdown
func serve(configPath, dataRoot, role string) error {
	// 1. Shared setup (runs for every role).
	rt, err := boot.Setup(configPath, dataRoot, role)
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

	// 2. Shared session service.
	sessSvc := boot.NewSessionService(rt)

	// 3. Engine + health check. Each role gets its own engine with the standard
	// middleware chain (Recovery → Trace → CORS); auth is applied per-route-group
	// inside the registration functions.
	engine := boot.NewEngine()
	handler.RegisterHealthz(engine)

	// Authenticated internal gRPC listener (extract-scheduler-service). The api partition
	// serves the SchedulerControlService, the agent partition serves the
	// ScheduledRuntimeService, and the combined role serves both on one
	// listener. The Scheduler calls these with its own service identity.
	var internalSrv *grpc.Server
	var internalLis net.Listener
	if rt.Cfg.InternalRPC.Enabled {
		var terr error
		internalSrv, internalLis, terr = boot.NewInternalRPCServer(rt.Cfg.InternalRPC)
		if terr != nil {
			return terr
		}
		if role != "agent" {
			apiserver.RegisterControlService(internalSrv, rt)
		}
		boot.LogInternalRPCSecurity(log, rt.Cfg.InternalRPC)
	}

	// 4. Register routes by role. Each Wire constructs only the handlers its
	// partition needs, so the api role never instantiates the orchestrator,
	// OpenAI client, tool registry, or MCP manager.
	var asm *agentserver.Assembly
	switch role {
	case "all":
		handler.RegisterAPIRoutes(engine, apiserver.Wire(rt, sessSvc))
		asm = agentserver.Wire(rt, sessSvc, internalSrv)
		defer asm.Close(log)
		handler.RegisterAgentRoutes(engine, asm.Routes)
	case "api":
		handler.RegisterAPIRoutes(engine, apiserver.Wire(rt, sessSvc))
	case "agent":
		asm = agentserver.Wire(rt, sessSvc, internalSrv)
		defer asm.Close(log)
		handler.RegisterAgentRoutes(engine, asm.Routes)
	}
	if internalSrv != nil {
		go func() {
			if serr := internalSrv.Serve(internalLis); serr != nil && !errors.Is(serr, grpc.ErrServerStopped) {
				log.Fatal("internal rpc server failed", zap.Error(serr))
			}
		}()
		log.Info("internal rpc listening",
			zap.String("role", role),
			zap.String("addr", rt.Cfg.InternalRPC.Listen))
	}

	// Staged-attachment orphan sweeper (add-staged-attachment-upload): staged
	// uploads never get a session-delete cleanup, so roles serving the API
	// partition run the TTL reaper. The agent role never does — in a split
	// deployment exactly one api node owns the sweep. Best-effort: the loop
	// logs and survives its own failures.
	if role != "agent" {
		stopSweep := apiserver.StartAttachmentSweeper(rt)
		defer stopSweep()
		log.Info("staged attachment sweeper started", zap.String("role", role))
	}

	// 5. Per-role listener. all and api listen on server.port; agent listens on
	// server.agent_port. D2: all keeps a single listener on server.port so any
	// existing caller (tests, dev proxy, monitoring) keeps working unchanged.
	port := rt.Cfg.Server.Port
	if role == "agent" {
		port = rt.Cfg.Server.AgentPort
	}

	log.Info("server starting",
		zap.String("role", role),
		zap.Int("port", port),
		zap.Strings("route_groups", boot.RouteGroups(role)))

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

	// Session message queue: gate new turns (and park the reconciler) before
	// anything else drains, so the shutdown-bound turn cancels never pump the
	// queued heads — entries survive in Redis for the post-restart reconciler.
	if asm != nil {
		asm.StopAdmission()
		log.Info("agent turn admission and scheduler stopped", zap.String("role", role))
	}

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
	log.Info("server stopped", zap.String("role", role))

	// Ordered agent drain: cancel/wait running turns (each persists its
	// partial output through the ordinary interrupted-turn path), drain the
	// raw-capture buffer, release the memory client. Nil asm (api role) skips.
	if asm != nil {
		asm.Finalize(log)
	}
	return nil
}

// resolveRole reads and validates the --role flag, rejecting unknown values
// before any setup runs so the process exits non-zero without side effects.
func validateRole(role string) error {
	if !slices.Contains(validRoles, role) {
		return fmt.Errorf("invalid --role %q (want %s)", role, strings.Join(validRoles, "|"))
	}
	return nil
}
