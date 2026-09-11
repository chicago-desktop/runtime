package registry

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type publicationRecoveryServer struct {
	historyv1.UnimplementedHistoryServiceServer
	firstRequest string
	mutations    []*historyv1.Mutation
	receiptReads int
	mu           sync.Mutex
}

func (s *publicationRecoveryServer) Submit(_ context.Context, request *historyv1.SubmitRequest) (*historyv1.SubmitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutations = append(s.mutations, request.Mutations...)
	if s.firstRequest == "" {
		s.firstRequest = request.RequestId
		return nil, status.Error(codes.Unavailable, "submit response unavailable")
	}
	return &historyv1.SubmitResponse{Receipt: &historyv1.Receipt{RequestId: request.RequestId, Revision: 4, PublishedRevision: 4, Status: "published"}}, nil
}

func (s *publicationRecoveryServer) GetReceipt(_ context.Context, request *historyv1.GetReceiptRequest) (*historyv1.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receiptReads++
	if s.receiptReads <= 2 {
		return nil, status.Error(codes.Unavailable, "receipt unavailable")
	}
	return &historyv1.Receipt{RequestId: request.RequestId, Revision: 1, PublishedRevision: 3, Status: "published"}, nil
}

func (s *publicationRecoveryServer) GetVersion(context.Context, *historyv1.GetRequest) (*historyv1.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &historyv1.Version{Revision: 4, Entries: s.mutations}, nil
}

func (s *publicationRecoveryServer) ReportApplied(context.Context, *historyv1.AppliedRequest) (*historyv1.Empty, error) {
	return &historyv1.Empty{}, nil
}

func TestAppliedPublicationUnblocksWriteAfterUnknownCommit(t *testing.T) {
	server := &publicationRecoveryServer{}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	historyv1.RegisterHistoryServiceServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///history", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	history, err := remote.New(connection, remote.Config{Key: &historyv1.RegistryKey{TenantId: "tenant", EnvironmentId: "stage", RegistryId: "registry"}, ReplicaID: "replica", Timeout: time.Second, PollInterval: time.Millisecond})
	require.NoError(t, err)
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	reg := NewRegistry(history, newApplyingMockRunner(builder), builder, resolver, zap.NewNop())
	first := regapi.Entry{ID: regapi.NewID("test", "first"), Kind: regapi.EntryKind, Data: payload.NewString("first")}
	_, err = reg.Apply(t.Context(), regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: first}})
	require.ErrorIs(t, err, remote.ErrCommitUnknown)
	require.Empty(t, reg.Snapshot().Entries)
	require.NoError(t, reg.applyPublication(t.Context(), history, &regapi.PublishedState{Version: version.New(3), Changes: regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: first}}}, nil, false))
	second := regapi.Entry{ID: regapi.NewID("test", "second"), Kind: regapi.EntryKind, Data: payload.NewString("second")}
	_, err = reg.Apply(t.Context(), regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: second}})
	require.NoError(t, err)
	require.ElementsMatch(t, regapi.State{first, second}, reg.Snapshot().Entries)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Equal(t, 3, server.receiptReads)
}
