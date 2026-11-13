package rpcx

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const (
	mainTestToken      = "main_rpc_test_credential_only"
	schedulerTestToken = "scheduler_rpc_test_credential_only"
)

func serveSecurityTestRPC(t *testing.T, srv *grpc.Server) *bufconn.Listener {
	t.Helper()
	schedulerv1.RegisterSchedulerServiceServer(srv, &fakeScheduler{})
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthServer)
	lis := bufconn.Listen(1 << 20)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		assert.NoError(t, lis.Close())
		err := <-done
		if err != nil {
			assert.True(t, errors.Is(err, grpc.ErrServerStopped), "unexpected server shutdown error: %v", err)
		}
	})
	return lis
}

func rawPlaintextTestConn(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(bufDialer(lis)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	return conn
}

func plaintextServerOptions() ServerOptions {
	return ServerOptions{
		SecurityMode:    SecurityModePlaintextToken,
		RequireDeadline: true,
		PeerTokens: map[string]string{
			IdentityMain:      mainTestToken,
			IdentityScheduler: schedulerTestToken,
		},
	}
}

func TestPlaintextTokenAuthorization(t *testing.T) {
	t.Parallel()
	srv := grpc.NewServer(grpc.UnaryInterceptor(UnaryServerInterceptor(plaintextServerOptions())))
	lis := serveSecurityTestRPC(t, srv)
	conn := rawPlaintextTestConn(t, lis)
	client := schedulerv1.NewSchedulerServiceClient(conn)
	tests := []struct {
		name       string
		headers    []string
		want       codes.Code
		noDeadline bool
	}{
		{name: "valid main service", headers: []string{"Bearer " + mainTestToken}, want: codes.OK},
		{name: "case insensitive scheme", headers: []string{"bEaReR " + mainTestToken}, want: codes.OK},
		{name: "missing credential", want: codes.Unauthenticated},
		{name: "wrong credential", headers: []string{"Bearer MAY_LEAK_SECRET_wrong"}, want: codes.Unauthenticated},
		{name: "wrong scheme", headers: []string{"Basic " + mainTestToken}, want: codes.Unauthenticated},
		{name: "empty bearer", headers: []string{"Bearer"}, want: codes.Unauthenticated},
		{name: "ambiguous authorization", headers: []string{"Bearer " + mainTestToken, "Bearer " + schedulerTestToken}, want: codes.Unauthenticated},
		{name: "scheduler cannot impersonate main", headers: []string{"Bearer " + schedulerTestToken}, want: codes.PermissionDenied},
		{name: "ordinary call requires deadline", headers: []string{"Bearer " + mainTestToken}, want: codes.InvalidArgument, noDeadline: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if !tt.noDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
			}
			ctx = metadata.NewOutgoingContext(ctx, metadata.MD{
				"authorization":       tt.headers,
				"x-blowball-identity": []string{IdentityMain},
				"x-internal-identity": []string{IdentityMain},
			})
			resp, err := client.GetTask(ctx, &schedulerv1.GetTaskRequest{
				Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
			})
			assert.Equal(t, tt.want, status.Code(err))
			if tt.want == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, "task-1", resp.GetTask().GetTaskId())
				assert.Equal(t, "user-1", resp.GetTask().GetUserId())
			}
			if err != nil {
				assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
				assert.NotContains(t, err.Error(), mainTestToken)
				assert.NotContains(t, err.Error(), schedulerTestToken)
			}
		})
	}
}

func TestPlaintextHealthAuthorization(t *testing.T) {
	t.Parallel()
	srv := grpc.NewServer(grpc.UnaryInterceptor(UnaryServerInterceptor(plaintextServerOptions())))
	conn := rawPlaintextTestConn(t, serveSecurityTestRPC(t, srv))
	tests := []struct {
		name   string
		header string
		want   codes.Code
	}{
		{name: "authenticated without deadline", header: "Bearer " + mainTestToken, want: codes.OK},
		{name: "scheduler health is exempt from method policy", header: "Bearer " + schedulerTestToken, want: codes.OK},
		{name: "anonymous health rejected", want: codes.Unauthenticated},
		{name: "wrong credential rejected", header: "Bearer MAY_LEAK_SECRET_wrong", want: codes.Unauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.header != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", tt.header)
			}
			_, hasDeadline := ctx.Deadline()
			require.False(t, hasDeadline)
			resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			assert.Equal(t, tt.want, status.Code(err))
			if tt.want == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
			}
		})
	}
}

func TestPlaintextHealthWatchAuthorization(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(plaintextServerOptions())
	require.NoError(t, err)
	conn := rawPlaintextTestConn(t, serveSecurityTestRPC(t, srv))
	tests := []struct {
		name    string
		headers []string
		want    codes.Code
	}{
		{name: "main without deadline", headers: []string{"Bearer " + mainTestToken}, want: codes.OK},
		{name: "scheduler without deadline", headers: []string{"Bearer " + schedulerTestToken}, want: codes.OK},
		{name: "anonymous", want: codes.Unauthenticated},
		{name: "wrong token", headers: []string{"Bearer MAY_LEAK_SECRET_wrong"}, want: codes.Unauthenticated},
		{name: "duplicate authorization", headers: []string{"Bearer " + mainTestToken, "Bearer " + schedulerTestToken}, want: codes.Unauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timer := time.AfterFunc(2*time.Second, cancel)
			defer timer.Stop()
			_, hasDeadline := ctx.Deadline()
			require.False(t, hasDeadline)
			ctx = metadata.NewOutgoingContext(ctx, metadata.MD{"authorization": tt.headers})
			stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
			var resp *healthpb.HealthCheckResponse
			if err == nil {
				resp, err = stream.Recv()
			}
			assert.Equal(t, tt.want, status.Code(err))
			if tt.want == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
			} else if err != nil {
				assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
				assert.NotContains(t, err.Error(), mainTestToken)
				assert.NotContains(t, err.Error(), schedulerTestToken)
			}
		})
	}
}

func TestPlaintextDialAndServerIgnoreTLSFiles(t *testing.T) {
	t.Parallel()
	opts := plaintextServerOptions()
	opts.TLS = TLSFiles{CertFile: "/missing/cert", KeyFile: "/missing/key", CAFile: "/missing/ca"}
	srv, err := NewServer(opts)
	require.NoError(t, err)
	// The listener must own a snapshot, not an externally mutable token map.
	opts.PeerTokens[IdentityMain] = "replaced_test_credential"
	lis := serveSecurityTestRPC(t, srv)
	conn, err := Dial(DialOptions{
		Target: "passthrough:///bufnet", SecurityMode: SecurityModePlaintextToken,
		ClientToken: mainTestToken, TLS: opts.TLS,
		Extra: []grpc.DialOption{grpc.WithContextDialer(bufDialer(lis))},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	resp, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(t.Context(),
		&schedulerv1.GetTaskRequest{Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1"})
	require.NoError(t, err)
	assert.Equal(t, "task-1", resp.GetTask().GetTaskId())
}

func TestTransportModesDoNotDowngrade(t *testing.T) {
	t.Parallel()
	t.Run("plaintext client cannot use mutual TLS listener", func(t *testing.T) {
		pki := newTestPKI(t)
		srv, err := NewServer(ServerOptions{TLS: pki.files(t, IdentityScheduler, true), RequireDeadline: true})
		require.NoError(t, err)
		lis := serveSecurityTestRPC(t, srv)
		conn, err := Dial(DialOptions{
			Target: "passthrough:///bufnet", SecurityMode: SecurityModePlaintextToken, ClientToken: mainTestToken,
			Extra: []grpc.DialOption{grpc.WithContextDialer(bufDialer(lis))},
		})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, conn.Close()) })
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = schedulerv1.NewSchedulerServiceClient(conn).GetTask(ctx, &schedulerv1.GetTaskRequest{TaskId: "task-1"})
		require.Error(t, err)
	})
	t.Run("mutual TLS client cannot use plaintext listener", func(t *testing.T) {
		pki := newTestPKI(t)
		srv, err := NewServer(plaintextServerOptions())
		require.NoError(t, err)
		lis := serveSecurityTestRPC(t, srv)
		conn, err := Dial(DialOptions{
			Target: "passthrough:///bufnet", TLS: pki.files(t, IdentityMain, false),
			Extra: []grpc.DialOption{grpc.WithContextDialer(bufDialer(lis))},
		})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, conn.Close()) })
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = schedulerv1.NewSchedulerServiceClient(conn).GetTask(ctx, &schedulerv1.GetTaskRequest{TaskId: "task-1"})
		require.Error(t, err)
	})
	t.Run("bearer does not replace mutual TLS client certificate", func(t *testing.T) {
		pki := newTestPKI(t)
		srv, err := NewServer(ServerOptions{TLS: pki.files(t, IdentityScheduler, true), RequireDeadline: true})
		require.NoError(t, err)
		lis := serveSecurityTestRPC(t, srv)
		pool, err := loadCAPool(pki.dir + "/ca.pem")
		require.NoError(t, err)
		conn := dialWithTLS(t, &tls.Config{RootCAs: pool, ServerName: "bufnet", MinVersion: tls.VersionTLS13}, bufDialer(lis), true)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+mainTestToken)
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		require.Error(t, err)
	})
}

func TestRPCCredentialsDoNotLeak(t *testing.T) {
	t.Parallel()
	invalid := "MAY_LEAK_SECRET invalid"
	_, err := NewServer(ServerOptions{SecurityMode: SecurityModePlaintextToken, PeerTokens: map[string]string{IdentityMain: invalid}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
	_, err = Dial(DialOptions{Target: "passthrough:///bufnet", SecurityMode: SecurityModePlaintextToken, ClientToken: invalid})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "MAY_LEAK_SECRET")
	_, err = NewServer(ServerOptions{SecurityMode: "auto"})
	require.Error(t, err)
	_, err = Dial(DialOptions{Target: "passthrough:///bufnet", SecurityMode: "auto"})
	require.Error(t, err)
}
