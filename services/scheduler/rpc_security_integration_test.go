//go:build integration

package scheduler_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/model"
	"github.com/lush/blowball/internal/modelselect"
	"github.com/lush/blowball/internal/rpcx"
	"github.com/lush/blowball/internal/scheduledcontract"
	"github.com/lush/blowball/internal/scheduledcontrol"
	"github.com/lush/blowball/internal/scheduledruntime"
	"github.com/lush/blowball/services/scheduler/internal/rpc"
	"github.com/lush/blowball/services/scheduler/internal/rpcclient"
	"github.com/lush/blowball/services/scheduler/internal/scheduler"
	"github.com/lush/blowball/services/scheduler/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

const (
	lifecycleMainToken      = "RPC_MAIN_TEST_CREDENTIAL_NOT_MODEL_DATA"
	lifecycleSchedulerToken = "RPC_SCHEDULER_TEST_CREDENTIAL_NOT_MODEL_DATA"
)

func rpcLifecycleTLS(t *testing.T) map[string]rpcx.TLSFiles {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "RPC lifecycle test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caPath := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	files := make(map[string]rpcx.TLSFiles, 2)
	for index, identity := range []string{rpcx.IdentityMain, rpcx.IdentityScheduler} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(int64(index + 2)), Subject: pkix.Name{CommonName: identity},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: []string{"bufnet"},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		certPath, keyPath := filepath.Join(dir, identity+".crt"), filepath.Join(dir, identity+".key")
		require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
		require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
		files[identity] = rpcx.TLSFiles{CertFile: certPath, KeyFile: keyPath, CAFile: caPath, ServerName: "bufnet"}
	}
	return files
}

func lifecycleDial(t *testing.T, mode rpcx.SecurityMode, files rpcx.TLSFiles, token string, listener *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	conn, err := rpcx.Dial(rpcx.DialOptions{
		Target: "passthrough:///bufnet", SecurityMode: mode, TLS: files, ClientToken: token,
		Extra: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		})},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	return conn
}

func startLifecycleRPC(t *testing.T, server *grpc.Server, listener *bufconn.Listener) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		assert.NoError(t, listener.Close())
		<-done
	})
}

func TestScheduledRPCSecurityLifecycle(t *testing.T) {
	for _, mode := range []rpcx.SecurityMode{rpcx.SecurityModeMTLS, rpcx.SecurityModePlaintextToken} {
		t.Run(string(mode), func(t *testing.T) {
			schedStore, mainStore := setupIntegration(t)
			files := map[string]rpcx.TLSFiles{
				rpcx.IdentityMain:      {CertFile: "/missing/cert", KeyFile: "/missing/key", CAFile: "/missing/ca"},
				rpcx.IdentityScheduler: {CertFile: "/missing/cert", KeyFile: "/missing/key", CAFile: "/missing/ca"},
			}
			if mode == rpcx.SecurityModeMTLS {
				files = rpcLifecycleTLS(t)
			}
			schedulerListener, mainListener := bufconn.Listen(1<<20), bufconn.Listen(1<<20)
			mainConn := lifecycleDial(t, mode, files[rpcx.IdentityMain], lifecycleMainToken, schedulerListener)
			schedulerConn := lifecycleDial(t, mode, files[rpcx.IdentityScheduler], lifecycleSchedulerToken, mainListener)
			mainClient := schedulerv1.NewSchedulerServiceClient(mainConn)
			selection := modelselect.Config{
				Catalog: []config.ModelCatalogEntry{{Name: "model-a", MaxContextTokens: 100000, MaxCompletionTokens: 8000}},
				Default: "model-a", DefaultEffort: "none",
			}
			turns := &fakeTurn{}
			rt, err := scheduledruntime.New(scheduledruntime.Dependencies{
				Store: mainStore, Occurrences: scheduledruntime.NewSchedulerClient(mainClient),
				Users: echoUsers{}, Turns: turns, Tokens: fakeTokens{},
				Canceller: &fakeCanceller{release: make(chan struct{})}, Selection: selection,
				Config: scheduledruntime.Config{PollInterval: 20 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
					MaxConcurrent: 2, MaxPrepareAttempts: 3, ScanBatch: 10, DefaultRunDuration: time.Second},
			})
			require.NoError(t, err)
			tasks := task.New(schedStore, rpcclient.NewControlClient(schedulerv1.NewSchedulerControlServiceClient(schedulerConn)), 5000)
			sched := scheduler.New(schedStore, rpcclient.NewRuntimeClient(scheduledruntimev1.NewScheduledRuntimeServiceClient(schedulerConn)),
				scheduler.Config{PollInterval: 20 * time.Millisecond, LeaseDuration: 5 * time.Second,
					MaxRunDuration: 2 * time.Second, MaxConcurrent: 2, ScanBatch: 10, MaxAttempts: 3,
					RetryBackoff: 10 * time.Millisecond, ExecutionEnabled: true})
			h := &harness{schedStore: schedStore, mainStore: mainStore, tasks: tasks, sched: sched, runtime: rt, turns: turns}
			mainServer, err := rpcx.NewServer(rpcx.ServerOptions{
				SecurityMode: mode, TLS: files[rpcx.IdentityMain], RequireDeadline: true,
				PeerTokens: map[string]string{rpcx.IdentityScheduler: lifecycleSchedulerToken},
			})
			require.NoError(t, err)
			schedulerv1.RegisterSchedulerControlServiceServer(mainServer, scheduledcontrol.New(echoUsers{}, nil, selection))
			scheduledruntimev1.RegisterScheduledRuntimeServiceServer(mainServer, scheduledruntime.NewService(rt))
			schedulerServer, err := rpcx.NewServer(rpcx.ServerOptions{
				SecurityMode: mode, TLS: files[rpcx.IdentityScheduler], RequireDeadline: true,
				PeerTokens: map[string]string{rpcx.IdentityMain: lifecycleMainToken, rpcx.IdentityScheduler: lifecycleSchedulerToken},
			})
			require.NoError(t, err)
			schedulerv1.RegisterSchedulerServiceServer(schedulerServer, rpc.NewServer(tasks, sched))
			startLifecycleRPC(t, mainServer, mainListener)
			startLifecycleRPC(t, schedulerServer, schedulerListener)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			created, err := mainClient.CreateTask(ctx, &schedulerv1.CreateTaskRequest{
				Actor: &schedulerv1.Actor{UserId: "user-1"},
				Config: &schedulerv1.TaskConfig{Name: "RPC lifecycle", Prompt: "do it", Enabled: true,
					Schedule: &schedulerv1.Schedule{Type: schedulerv1.ScheduleType_SCHEDULE_TYPE_CRON,
						CronExpression: "* * * * *", Timezone: "UTC"}},
			})
			require.NoError(t, err)
			taskID := created.GetTask().GetTaskId()
			_, err = mainClient.GetTask(ctx, &schedulerv1.GetTaskRequest{Actor: &schedulerv1.Actor{UserId: "user-2"}, TaskId: taskID})
			require.Error(t, err, "transport authentication must not bypass task ownership")
			imposterConn := lifecycleDial(t, mode, files[rpcx.IdentityScheduler], lifecycleSchedulerToken, schedulerListener)
			imposterCtx := metadata.AppendToOutgoingContext(ctx, "x-blowball-identity", rpcx.IdentityMain)
			_, err = schedulerv1.NewSchedulerServiceClient(imposterConn).GetTask(imposterCtx,
				&schedulerv1.GetTaskRequest{Actor: &schedulerv1.Actor{UserId: "user-1"}, TaskId: taskID})
			require.Error(t, err)
			domain, ok := rpcx.AsError(err)
			require.True(t, ok)
			assert.Equal(t, rpcx.CodePermissionDenied, domain.Code)
			loaded, err := schedStore.GetTaskInternal(ctx, taskID)
			require.NoError(t, err)
			due := time.Now().UTC().Add(-time.Minute)
			loaded.NextRunAt = &due
			_, err = schedStore.UpdateTask(ctx, loaded, loaded.Revision)
			require.NoError(t, err)
			h.start()
			t.Cleanup(h.stop)
			require.True(t, h.waitForStatus(ctx, taskID, model.ScheduledRunSucceeded, 10*time.Second), "RPC execution did not succeed")
			occurrence := h.firstOccurrence(t, taskID)
			receipt, err := mainStore.GetScheduledExecution(ctx, occurrence.OccurrenceID)
			require.NoError(t, err)
			require.NotNil(t, receipt)
			assert.True(t, receipt.Terminal())
			grant, err := mainClient.GetStartGrant(ctx, &schedulerv1.GetStartGrantRequest{
				OccurrenceId: occurrence.OccurrenceID, ReceiptGeneration: receipt.ReceiptGeneration,
			})
			require.NoError(t, err)
			require.NotNil(t, grant.GetGrant())
			fingerprint := scheduledcontract.Fingerprint(scheduledcontract.FingerprintInput{
				TaskID: taskID, OccurrenceID: occurrence.OccurrenceID, RunID: occurrence.RunID,
				SessionID: occurrence.SessionID, SnapshotJSON: occurrence.SnapshotJSON,
				ResourceRevision: occurrence.ResourceRevision, ManifestHash: occurrence.ManifestHash,
			})
			submit := &scheduledruntimev1.SubmitScheduledRunRequest{
				TaskId: taskID, OccurrenceId: occurrence.OccurrenceID, RunId: occurrence.RunID,
				SessionId: occurrence.SessionID, Attempt: uint32(occurrence.AttemptCount), ConfigFingerprint: fingerprint,
			}
			runtimeClient := scheduledruntimev1.NewScheduledRuntimeServiceClient(schedulerConn)
			replayed, err := runtimeClient.SubmitScheduledRun(ctx, submit)
			require.NoError(t, err)
			assert.True(t, replayed.GetAlreadyAccepted())
			submit.ConfigFingerprint = "forged"
			_, err = runtimeClient.SubmitScheduledRun(ctx, submit)
			require.Error(t, err, "authentication must not bypass authoritative configuration validation")
			request := turns.LastRequest()
			require.NotNil(t, request)
			assert.Equal(t, "do it", request.Content)
			for _, token := range []string{lifecycleMainToken, lifecycleSchedulerToken} {
				assert.NotContains(t, fmt.Sprintf("%+v", request), token)
				assert.NotContains(t, occurrence.SnapshotJSON, token)
				assert.NotContains(t, fmt.Sprintf("%+v", receipt), token)
			}
		})
	}
}
