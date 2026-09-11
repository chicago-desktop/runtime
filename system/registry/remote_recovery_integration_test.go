//go:build historyintegration

package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	"github.com/wippyai/runtime/system/registry/history/remote"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestRemoteRecoveryAcrossProcesses(t *testing.T) {
	if os.Getenv("WIPPY_HISTORY_RECOVERY_ENDPOINT") == "" {
		t.Fatal("WIPPY_HISTORY_RECOVERY_ENDPOINT is required")
	}
	prefix := uuid.NewString()
	executable, err := os.Executable()
	require.NoError(t, err)
	writer := exec.Command(executable, "-test.run=^TestRemoteRecoveryProcess$")
	writer.Dir = t.TempDir()
	writer.Env = append(os.Environ(), "WIPPY_HISTORY_RECOVERY_PHASE=write", "WIPPY_HISTORY_RECOVERY_PREFIX="+prefix)
	output, err := writer.CombinedOutput()
	require.NoError(t, err, string(output))
	var receipt regapi.HistoryReceipt
	require.NoError(t, json.Unmarshal(output, &receipt), string(output))
	require.NotEmpty(t, receipt.RequestID)
	reader := exec.Command(executable, "-test.run=^TestRemoteRecoveryProcess$")
	reader.Dir = t.TempDir()
	reader.Env = append(os.Environ(), "WIPPY_HISTORY_RECOVERY_PHASE=read", "WIPPY_HISTORY_RECOVERY_PREFIX="+prefix, "WIPPY_HISTORY_RECOVERY_REQUEST="+receipt.RequestID)
	output, err = reader.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestRemoteRecoveryProcess(t *testing.T) {
	phase := os.Getenv("WIPPY_HISTORY_RECOVERY_PHASE")
	if phase == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := remote.Config{Key: &historyv1.RegistryKey{TenantId: os.Getenv("WIPPY_HISTORY_RECOVERY_TENANT"), EnvironmentId: os.Getenv("WIPPY_HISTORY_RECOVERY_ENVIRONMENT"), RegistryId: os.Getenv("WIPPY_HISTORY_RECOVERY_REGISTRY")}, ReplicaID: "recovery-" + os.Getenv("WIPPY_HISTORY_RECOVERY_PREFIX"), Timeout: 5 * time.Second, PollInterval: 10 * time.Millisecond}
	ca, err := os.ReadFile(os.Getenv("WIPPY_HISTORY_RECOVERY_CA"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	certificate, err := tls.LoadX509KeyPair(os.Getenv("WIPPY_HISTORY_RECOVERY_CERT"), os.Getenv("WIPPY_HISTORY_RECOVERY_KEY"))
	require.NoError(t, err)
	token, err := os.ReadFile(os.Getenv("WIPPY_HISTORY_RECOVERY_TOKEN"))
	require.NoError(t, err)
	var loseResponse atomic.Bool
	connection, err := grpc.NewClient(os.Getenv("WIPPY_HISTORY_RECOVERY_ENDPOINT"), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}, ServerName: os.Getenv("WIPPY_HISTORY_RECOVERY_SERVER_NAME")})), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
		err := invoke(ctx, method, request, reply, connection, options...)
		if err == nil && method == historyv1.HistoryService_Submit_FullMethodName && loseResponse.CompareAndSwap(true, false) {
			return status.Error(codes.Unavailable, "test removed the commit response")
		}
		return err
	}))
	require.NoError(t, err)
	defer connection.Close()
	history, err := remote.New(connection, cfg)
	require.NoError(t, err)
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	reg := NewRegistry(history, newApplyingMockRunner(builder), builder, resolver, zap.NewNop())
	prefix := os.Getenv("WIPPY_HISTORY_RECOVERY_PREFIX")
	retained := regapi.Entry{ID: regapi.NewID(prefix, "baseline"), Kind: regapi.EntryKind, Data: payload.NewString("retained"), Registry: regapi.EntryMetadata{Owner: "test/deployment"}}
	deleted := regapi.Entry{ID: regapi.NewID(prefix, "deleted"), Kind: regapi.EntryKind}
	dynamic := regapi.Entry{ID: regapi.NewID(prefix, "dynamic"), Kind: regapi.EntryKind, Data: payload.NewString("durable")}
	if phase == "write" {
		require.NoError(t, reg.LoadState(ctx, regapi.State{retained, deleted}, version.New(0)))
		loseResponse.Store(true)
		receipt, err := history.SubmitChanges(ctx, regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: retained}, {Kind: regapi.EntryDelete, Entry: deleted}, {Kind: regapi.EntryCreate, Entry: dynamic}}, nil)
		require.NoError(t, err)
		_, err = reg.GetEntry(dynamic.ID)
		require.Error(t, err)
		encoded, err := json.Marshal(receipt)
		require.NoError(t, err)
		fmt.Print(string(encoded))
		os.Exit(0)
	}
	require.Equal(t, "read", phase)
	_, err = history.AwaitPublished(ctx, &regapi.HistoryReceipt{RequestID: os.Getenv("WIPPY_HISTORY_RECOVERY_REQUEST"), Status: "stored"})
	require.NoError(t, err)
	require.NoError(t, reg.LoadState(ctx, nil, version.New(0)))
	loaded, err := reg.GetEntry(retained.ID)
	require.NoError(t, err)
	require.Equal(t, retained, loaded)
	loaded, err = reg.GetEntry(dynamic.ID)
	require.NoError(t, err)
	require.Equal(t, dynamic, loaded)
	_, err = reg.GetEntry(deleted.ID)
	require.Error(t, err)
	dynamic.Data = payload.NewString("after restart")
	_, err = reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: dynamic}})
	require.NoError(t, err)
	loaded, err = reg.GetEntry(dynamic.ID)
	require.NoError(t, err)
	require.Equal(t, dynamic, loaded)
	_, err = os.Stat("wippy.lock")
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(".wippy/registry.db")
	require.True(t, os.IsNotExist(err))
}
