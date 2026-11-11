package rpcx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
)

// testPKI is an ephemeral CA used only by these tests. It writes the CA,
// server and per-identity client material into a temp dir so TLSFiles is
// exercised exactly like production configuration.
type testPKI struct {
	dir    string
	caPEM  []byte
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "blowball-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), caPEM, 0o600))
	return &testPKI{dir: dir, caPEM: caPEM, caCert: cert, caKey: key}
}

// files issues a leaf certificate with CommonName=identity and returns the
// TLSFiles pointing at it. server adds the bufconn DNS name plus ServerAuth.
func (p *testPKI) files(t *testing.T, identity string, server bool) TLSFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: identity},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"bufnet"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	certPath := filepath.Join(p.dir, identity+"-cert.pem")
	keyPath := filepath.Join(p.dir, identity+"-key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	files := TLSFiles{CertFile: certPath, KeyFile: keyPath, CAFile: filepath.Join(p.dir, "ca.pem")}
	if !server {
		files.ServerName = "bufnet"
	}
	return files
}

func startTLSServer(t *testing.T, files TLSFiles, register func(*grpc.Server)) *bufconn.Listener {
	t.Helper()
	tlsCfg, err := ServerTLS(files)
	require.NoError(t, err)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(UnaryServerInterceptor(ServerOptions{RequireDeadline: true})),
	)
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis
}

func bufDialer(lis *bufconn.Listener) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
}

func dialTest(t *testing.T, pki *testPKI, identity string, withClientInterceptor bool, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	tlsCfg, err := ClientTLS(pki.files(t, identity, false))
	require.NoError(t, err)
	return dialWithTLS(t, tlsCfg, bufDialer(lis), withClientInterceptor)
}

func dialWithTLS(t *testing.T, tlsCfg *tls.Config, dialer func(context.Context, string) (net.Conn, error), withClientInterceptor bool) *grpc.ClientConn {
	t.Helper()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
	if dialer != nil {
		opts = append(opts, grpc.WithContextDialer(dialer))
	}
	if withClientInterceptor {
		opts = append(opts, grpc.WithChainUnaryInterceptor(UnaryClientInterceptor(2*time.Second)))
	}
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type fakeScheduler struct {
	schedulerv1.UnimplementedSchedulerServiceServer
}

func (f *fakeScheduler) GetTask(_ context.Context, req *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	return &schedulerv1.GetTaskResponse{Task: &schedulerv1.Task{TaskId: req.GetTaskId(), UserId: req.GetActor().GetUserId()}}, nil
}

func newSchedulerServer(t *testing.T) (*testPKI, *bufconn.Listener) {
	t.Helper()
	pki := newTestPKI(t)
	lis := startTLSServer(t, pki.files(t, "blowball-test-server", true), func(srv *grpc.Server) {
		schedulerv1.RegisterSchedulerServiceServer(srv, &fakeScheduler{})
		scheduledruntimev1.RegisterScheduledRuntimeServiceServer(srv, &scheduledruntimev1.UnimplementedScheduledRuntimeServiceServer{})
		healthpb.RegisterHealthServer(srv, health.NewServer())
	})
	return pki, lis
}

func TestServiceIdentityAuthorization(t *testing.T) {
	pki, lis := newSchedulerServer(t)

	t.Run("allowed identity", func(t *testing.T) {
		conn := dialTest(t, pki, IdentityAPI, true, lis)
		resp, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{
			Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
		})
		require.NoError(t, err)
		assert.Equal(t, "task-1", resp.GetTask().GetTaskId())
	})

	t.Run("agent identity allowed for management tools", func(t *testing.T) {
		conn := dialTest(t, pki, IdentityAgent, true, lis)
		_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{
			Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: "task-1",
		})
		require.NoError(t, err)
	})

	t.Run("scheduler identity denied for task management", func(t *testing.T) {
		conn := dialTest(t, pki, IdentityScheduler, true, lis)
		_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{
			Actor: &schedulerv1.Actor{UserId: "forged-user"}, TaskId: "task-1",
		})
		require.Error(t, err)
		de, ok := AsError(err)
		require.True(t, ok)
		assert.Equal(t, CodePermissionDenied, de.Code)
	})

	t.Run("unknown identity denied", func(t *testing.T) {
		conn := dialTest(t, pki, "stranger", true, lis)
		_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{TaskId: "t"})
		require.Error(t, err)
		de, ok := AsError(err)
		require.True(t, ok)
		assert.Equal(t, CodePermissionDenied, de.Code)
	})
}

func TestMissingClientCertificateRejected(t *testing.T) {
	pki, lis := newSchedulerServer(t)
	// Root-only client TLS: no certificate to present.
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pki.caPEM))
	conn := dialWithTLS(t, &tls.Config{RootCAs: pool, ServerName: "bufnet", MinVersion: tls.VersionTLS13}, bufDialer(lis), true)
	_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{TaskId: "t"})
	require.Error(t, err)
	// The handshake failure surfaces as a transport error; what matters is
	// that no handler response was returned.
}

func TestMissingDeadlineRejected(t *testing.T) {
	pki, lis := newSchedulerServer(t)
	// No client interceptor: the caller's context has no deadline.
	conn := dialTest(t, pki, IdentityAPI, false, lis)
	_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{TaskId: "t"})
	require.Error(t, err)
	de, ok := AsError(FromStatus(err))
	require.True(t, ok)
	assert.Equal(t, CodeInvalidArgument, de.Code)
}

func TestHealthExemptFromDeadlineAndPolicy(t *testing.T) {
	pki, lis := newSchedulerServer(t)
	conn := dialTest(t, pki, "stranger", false, lis)
	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestPeerUnavailableIsUnavailable(t *testing.T) {
	pki := newTestPKI(t)
	lis := bufconn.Listen(1 << 20)
	require.NoError(t, lis.Close()) // no listener behind the dialer

	tlsCfg, err := ClientTLS(pki.files(t, IdentityAPI, false))
	require.NoError(t, err)
	deadConn := dialWithTLS(t, tlsCfg, bufDialer(lis), true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = schedulerv1.NewSchedulerServiceClient(deadConn).GetTask(ctx, &schedulerv1.GetTaskRequest{TaskId: "t"})
	require.Error(t, err)
	de, ok := AsError(err)
	require.True(t, ok)
	assert.Contains(t, []string{CodeUnavailable, CodeDeadlineExceeded}, de.Code)
}

// TestDefaultAuthorizerCoversEveryMethod guards the policy table: adding a
// proto RPC without an allowlist entry would make it permanently unreachable
// (fail closed), so the omission must be caught by a test instead of an
// outage.
func TestDefaultAuthorizerCoversEveryMethod(t *testing.T) {
	auth := DefaultAuthorizer()
	services := []grpc.ServiceDesc{
		schedulerv1.SchedulerService_ServiceDesc,
		schedulerv1.SchedulerControlService_ServiceDesc,
		scheduledruntimev1.ScheduledRuntimeService_ServiceDesc,
	}
	for _, svc := range services {
		for _, m := range svc.Methods {
			full := "/" + svc.ServiceName + "/" + m.MethodName
			assert.NotEmpty(t, auth[full], "missing authorizer entry for %s", full)
		}
	}
	for method, identities := range auth {
		assert.NotEmpty(t, identities, "empty allowlist for %s", method)
		for _, id := range identities {
			assert.Contains(t,
				[]string{IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain}, id,
				"method %s allows unknown identity %s", method, id)
		}
	}
}

func TestStatusCodesRoundTripThroughGeneratedService(t *testing.T) {
	pki := newTestPKI(t)
	lis := startTLSServer(t, pki.files(t, "blowball-test-server", true), func(srv *grpc.Server) {
		schedulerv1.RegisterSchedulerServiceServer(srv, &failingScheduler{})
	})
	conn := dialTest(t, pki, IdentityAPI, true, lis)
	_, err := schedulerv1.NewSchedulerServiceClient(conn).GetTask(context.Background(), &schedulerv1.GetTaskRequest{TaskId: "t"})
	require.Error(t, err)
	de, ok := AsError(err)
	require.True(t, ok)
	assert.Equal(t, CodeNotFound, de.Code)
	assert.Equal(t, "task not found", de.Detail)
}

type failingScheduler struct {
	schedulerv1.UnimplementedSchedulerServiceServer
}

func (f *failingScheduler) GetTask(context.Context, *schedulerv1.GetTaskRequest) (*schedulerv1.GetTaskResponse, error) {
	return nil, ToStatus(Errorf(CodeNotFound, "task not found"))
}
