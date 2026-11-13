// Command scheduler runs the standalone blowball Scheduler process. It owns
// task definitions, time-rule materialization, start authorization and the
// scheduling projection; it never constructs an Agent, loads an LLM key or
// mounts a user workspace.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/pkg/boot"
	"github.com/lush/blowball/pkg/logger"
	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/services/scheduler/internal/config"
	"github.com/lush/blowball/services/scheduler/internal/rpc"
	"github.com/lush/blowball/services/scheduler/internal/rpcclient"
	"github.com/lush/blowball/services/scheduler/internal/scheduler"
	"github.com/lush/blowball/services/scheduler/internal/store"
	"github.com/lush/blowball/services/scheduler/internal/task"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(boot.VersionString())
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scheduler: %v\n", err)
		os.Exit(1)
	}
}

func schedulerRPCOptions(cfg config.Config, endpoint string) (rpcx.ServerOptions, rpcx.DialOptions) {
	return rpcx.ServerOptions{
		SecurityMode: cfg.RPC.SecurityMode, TLS: cfg.RPC.TLS, PeerTokens: cfg.RPC.PeerTokens, RequireDeadline: true,
	}, rpcx.DialOptions{
		Target: endpoint, SecurityMode: cfg.RPC.SecurityMode, TLS: cfg.MainService.TLS,
		ClientToken: cfg.MainService.ClientToken, Timeout: cfg.MainService.Timeout,
	}
}

func logSchedulerRPCSecurity(log *zap.Logger, cfg config.Config) {
	if cfg.RPC.SecurityMode == rpcx.SecurityModePlaintextToken {
		log.Warn("internal rpc plaintext mode enabled: traffic is unencrypted and server identity is not verified",
			zap.String("security_mode", string(cfg.RPC.SecurityMode)), zap.String("listen", cfg.RPC.Listen),
			zap.String("api_endpoint", cfg.MainService.APIEndpoint), zap.String("agent_endpoint", cfg.MainService.AgentEndpoint))
	}
}

func run() error {
	configPath := "config.yaml"
	dataDir := "."
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-f", "--config":
			if i+1 < len(os.Args) {
				configPath = os.Args[i+1]
				i++
			}
		case "-d", "--data-dir":
			if i+1 < len(os.Args) {
				dataDir = os.Args[i+1]
				i++
			}
		default:
			return fmt.Errorf("unknown argument %q", os.Args[i])
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("scheduler: create log dir: %w", err)
	}
	log, err := logger.InitForRole(cfg.Logging, logDir, "scheduler")
	if err != nil {
		return fmt.Errorf("scheduler: init logger: %w", err)
	}
	log.Info("scheduler starting", zap.String("role", "scheduler"), zap.String("listen", cfg.RPC.Listen))

	st, err := store.New(cfg.MySQL.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	_, apiOpts := schedulerRPCOptions(*cfg, cfg.MainService.APIEndpoint)
	apiConn, err := rpcx.Dial(apiOpts)
	if err != nil {
		return fmt.Errorf("scheduler: dial main api rpc: %w", err)
	}
	defer apiConn.Close()
	agentConn := apiConn
	if cfg.MainService.AgentEndpoint != cfg.MainService.APIEndpoint {
		_, agentOpts := schedulerRPCOptions(*cfg, cfg.MainService.AgentEndpoint)
		agentConn, err = rpcx.Dial(agentOpts)
		if err != nil {
			return fmt.Errorf("scheduler: dial main agent rpc: %w", err)
		}
		defer agentConn.Close()
	}

	control := rpcclient.NewControlClient(schedulerv1.NewSchedulerControlServiceClient(apiConn))
	runtimeClient := rpcclient.NewRuntimeClient(scheduledruntimev1.NewScheduledRuntimeServiceClient(agentConn))

	enabled, poll, maxRun, lease, backoff, concurrent, attempts, scan, maxInput := cfg.Scheduler.Values()
	tasks := task.New(st, control, maxInput)
	sched := scheduler.New(st, runtimeClient, scheduler.Config{
		PollInterval: poll, LeaseDuration: lease, MaxRunDuration: maxRun,
		MaxConcurrent: concurrent, ScanBatch: scan, MaxAttempts: attempts,
		RetryBackoff: backoff, ExecutionEnabled: enabled,
	})

	serverOpts, _ := schedulerRPCOptions(*cfg, cfg.MainService.APIEndpoint)
	grpcServer, err := rpcx.NewServer(serverOpts)
	if err != nil {
		return fmt.Errorf("scheduler: server security: %w", err)
	}
	schedulerv1.RegisterSchedulerServiceServer(grpcServer, rpc.NewServer(tasks, sched))
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	lis, err := net.Listen("tcp", cfg.RPC.Listen)
	if err != nil {
		return fmt.Errorf("scheduler: listen %s: %w", cfg.RPC.Listen, err)
	}
	logSchedulerRPCSecurity(log, *cfg)
	serveErr := make(chan error, 1)
	go func() {
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serveErr <- err
		}
	}()
	sched.Start()
	log.Info("scheduler ready", zap.Bool("execution_enabled", enabled))

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		log.Info("scheduler shutdown requested", zap.String("signal", sig.String()))
	case err := <-serveErr:
		log.Error("scheduler grpc server failed", zap.Error(err))
		return err
	}

	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sched.Shutdown(shutdownCtx); err != nil {
		log.Warn("scheduler loop shutdown timed out", zap.Error(err))
	}
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		grpcServer.Stop()
	}
	log.Info("scheduler stopped")
	return nil
}
