package remote

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/registry"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type testServer struct {
	historyv1.UnimplementedHistoryServiceServer
	submits []*historyv1.SubmitRequest
	mu      sync.Mutex
	lost    bool
}

func (s *testServer) GetCandidate(context.Context, *historyv1.GetRequest) (*historyv1.Candidate, error) {
	return nil, status.Error(codes.NotFound, "empty registry")
}
func (s *testServer) Submit(_ context.Context, req *historyv1.SubmitRequest) (*historyv1.SubmitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits = append(s.submits, req)
	if s.lost {
		s.lost = false
		return nil, status.Error(codes.Unavailable, "response lost")
	}
	return &historyv1.SubmitResponse{Receipt: &historyv1.Receipt{RequestId: req.RequestId, Revision: 1, Status: "stored"}}, nil
}
func (s *testServer) GetReceipt(_ context.Context, req *historyv1.GetReceiptRequest) (*historyv1.Receipt, error) {
	return &historyv1.Receipt{RequestId: req.RequestId, Revision: 1, PublishedRevision: 3, Status: "published"}, nil
}
func (s *testServer) GetVersion(context.Context, *historyv1.GetRequest) (*historyv1.Version, error) {
	return &historyv1.Version{Revision: 3, Context: []*historyv1.Dot{{Actor: "other", Counter: 4}}}, nil
}
func (s *testServer) ReportApplied(context.Context, *historyv1.AppliedRequest) (*historyv1.Empty, error) {
	return &historyv1.Empty{}, nil
}

func newTestHistory(t testing.TB, server historyv1.HistoryServiceServer) *History {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	historyv1.RegisterHistoryServiceServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///history", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { connection.Close() })
	history, err := New(connection, Config{Key: &historyv1.RegistryKey{TenantId: "tenant", EnvironmentId: "stage", RegistryId: "registry"}, ReplicaID: "replica", Timeout: time.Second, PollInterval: time.Millisecond})
	require.NoError(t, err)
	return history
}

func TestLostResponseUsesOriginalReceipt(t *testing.T) {
	server := &testServer{lost: true}
	history := newTestHistory(t, server)
	receipt, err := history.SubmitChanges(context.Background(), registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Revision)
	published, err := history.AwaitPublished(context.Background(), receipt)
	require.NoError(t, err)
	require.Equal(t, uint(3), published.Version.ID())
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.submits, 1)
	require.Equal(t, server.submits[0].RequestId, receipt.RequestID)
}

func TestOnlyAppliedVersionsAdvanceCausalContext(t *testing.T) {
	server := &testServer{}
	history := newTestHistory(t, server)
	published, err := history.ReadPublished(context.Background(), 0)
	require.NoError(t, err)
	changes := registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}
	_, err = history.SubmitChanges(context.Background(), changes, nil)
	require.NoError(t, err)
	require.NoError(t, history.ReportApplied(context.Background(), published, nil))
	_, err = history.SubmitChanges(context.Background(), changes, nil)
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Empty(t, server.submits[0].Context)
	require.Equal(t, []*historyv1.Dot{{Actor: "other", Counter: 4}, {Actor: "replica", Counter: 1}}, server.submits[1].Context)
	require.Equal(t, uint64(2), server.submits[1].Dot.Counter)
	require.Equal(t, server.submits[0].Dot.Actor, server.submits[1].Dot.Actor)
}

func TestLegacyWritesFailExplicitly(t *testing.T) {
	history := newTestHistory(t, &testServer{})
	require.ErrorIs(t, history.Save(nil, nil, true), registry.ErrHistoryOperationUnsupported)
	require.ErrorIs(t, history.SetHead(nil), registry.ErrHistoryOperationUnsupported)
}

type unavailableReceiptServer struct {
	*testServer
}

func (s *unavailableReceiptServer) GetReceipt(context.Context, *historyv1.GetReceiptRequest) (*historyv1.Receipt, error) {
	return nil, status.Error(codes.Unavailable, "receipt unavailable")
}

func TestUnknownCommitRetainsRequestIdentity(t *testing.T) {
	server := &unavailableReceiptServer{testServer: &testServer{lost: true}}
	history := newTestHistory(t, server)
	changes := registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}
	_, err := history.SubmitChanges(context.Background(), changes, nil)
	require.ErrorIs(t, err, ErrCommitUnknown)
	_, err = history.SubmitChanges(context.Background(), registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "different")}}}, nil)
	require.ErrorIs(t, err, ErrCommitUnknown)
	_, err = history.SubmitChanges(context.Background(), changes, nil)
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.submits, 2)
	require.Equal(t, server.submits[0], server.submits[1])
}

type watchServer struct {
	historyv1.UnimplementedHistoryServiceServer
	cursors []uint64
	mu      sync.Mutex
}

func (s *watchServer) Watch(req *historyv1.ReadRequest, stream grpc.ServerStreamingServer[historyv1.Version]) error {
	s.mu.Lock()
	s.cursors = append(s.cursors, req.AfterRevision)
	revision := uint64(3)
	if len(s.cursors) > 1 {
		revision = 4
	}
	s.mu.Unlock()
	if err := stream.Send(&historyv1.Version{Revision: revision}); err != nil {
		return err
	}
	return status.Error(codes.Unavailable, "stream lost")
}

func TestWatchResumesAfterAppliedCursor(t *testing.T) {
	server := &watchServer{}
	history := newTestHistory(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var revisions []uint
	err := history.FollowPublished(ctx, 0, func(published *registry.PublishedState) error {
		revisions = append(revisions, published.Version.ID())
		if published.Version.ID() == 4 {
			cancel()
		}
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []uint{3, 4}, revisions)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Equal(t, []uint64{0, 3}, server.cursors)
}

func TestCanceledPublicationReturnsRequestIdentity(t *testing.T) {
	history := newTestHistory(t, &testServer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := history.AwaitPublished(ctx, &registry.HistoryReceipt{RequestID: "request", Status: "stored"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "request")
}

type restoreServer struct {
	*unavailableReceiptServer
	requests []*historyv1.RestoreRequest
}

func (s *restoreServer) Restore(_ context.Context, req *historyv1.RestoreRequest) (*historyv1.SubmitResponse, error) {
	s.requests = append(s.requests, req)
	if len(s.requests) == 1 {
		return nil, status.Error(codes.Unavailable, "restore response lost")
	}
	return &historyv1.SubmitResponse{Receipt: &historyv1.Receipt{RequestId: req.RequestId, Revision: 2, Status: "stored"}}, nil
}
func TestUnknownRestoreRetainsRequestIdentity(t *testing.T) {
	server := &restoreServer{unavailableReceiptServer: &unavailableReceiptServer{testServer: &testServer{}}}
	history := newTestHistory(t, server)
	_, err := history.RestoreChanges(context.Background(), 1)
	require.ErrorIs(t, err, ErrCommitUnknown)
	_, err = history.RestoreChanges(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, server.requests, 2)
	require.Equal(t, server.requests[0], server.requests[1])
}

type reportRetryServer struct {
	*testServer
	reports int
}

func (s *reportRetryServer) ReportApplied(context.Context, *historyv1.AppliedRequest) (*historyv1.Empty, error) {
	s.reports++
	if s.reports == 1 {
		return nil, status.Error(codes.Unavailable, "report response lost")
	}
	return &historyv1.Empty{}, nil
}
func TestFollowRetriesAppliedReportBeforeAdvancing(t *testing.T) {
	server := &reportRetryServer{testServer: &testServer{}}
	history := newTestHistory(t, server)
	published, err := history.ReadPublished(context.Background(), 0)
	require.NoError(t, err)
	require.Error(t, history.ReportApplied(context.Background(), published, nil))
	err = history.FollowPublished(context.Background(), 3, func(*registry.PublishedState) error { return nil })
	require.Equal(t, codes.Unimplemented, status.Code(err))
	require.Equal(t, 2, server.reports)
}

type actorServer struct {
	*testServer
	actorMu sync.Mutex
	counter uint64
	reads   int
}

func (s *actorServer) GetCandidate(context.Context, *historyv1.GetRequest) (*historyv1.Candidate, error) {
	s.actorMu.Lock()
	defer s.actorMu.Unlock()
	s.reads++
	if s.counter == 0 {
		return nil, status.Error(codes.NotFound, "empty registry")
	}
	return &historyv1.Candidate{Context: []*historyv1.Dot{{Actor: "replica", Counter: s.counter}, {Actor: "unapplied", Counter: 9}}}, nil
}

func (s *actorServer) Submit(ctx context.Context, request *historyv1.SubmitRequest) (*historyv1.SubmitResponse, error) {
	s.actorMu.Lock()
	defer s.actorMu.Unlock()
	var previous uint64
	for _, dot := range request.Context {
		if dot.Actor == "replica" {
			previous = dot.Counter
		}
	}
	if request.Dot.Actor != "replica" || request.Dot.Counter != s.counter+1 || previous != s.counter {
		return nil, status.Error(codes.InvalidArgument, "replica counter mismatch")
	}
	s.counter++
	return s.testServer.Submit(ctx, request)
}

func TestReplicaActorSurvivesRestartAfterLostResponse(t *testing.T) {
	server := &actorServer{testServer: &testServer{lost: true}}
	changes := registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}
	first := newTestHistory(t, server)
	_, err := first.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	second := newTestHistory(t, server)
	_, err = second.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	_, err = second.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.submits, 3)
	require.Equal(t, "replica", server.submits[2].Dot.Actor)
	require.Equal(t, uint64(3), server.submits[2].Dot.Counter)
	require.Equal(t, []*historyv1.Dot{{Actor: "replica", Counter: 1}}, server.submits[1].Context)
	require.Equal(t, 2, server.reads)
}

func TestConcurrentReplicaReuseDoesNotOverwriteCommittedDot(t *testing.T) {
	server := &actorServer{testServer: &testServer{}}
	changes := registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}
	first := newTestHistory(t, server)
	second := newTestHistory(t, server)
	_, err := first.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	_, err = second.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	for range 2 {
		_, err = first.SubmitChanges(t.Context(), changes, nil)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	require.Equal(t, uint64(2), server.counter)
	require.Len(t, server.submits, 2)
}

func TestRestartRetainsOwnPendingCausalPredecessor(t *testing.T) {
	server := &actorServer{testServer: &testServer{}}
	changes := registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: registry.NewID("test", "entry")}}}
	first := newTestHistory(t, server)
	published, err := first.ReadPublished(t.Context(), 0)
	require.NoError(t, err)
	require.NoError(t, first.ReportApplied(t.Context(), published, nil))
	_, err = first.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	second := newTestHistory(t, server)
	require.NoError(t, second.ReportApplied(t.Context(), published, nil))
	_, err = second.SubmitChanges(t.Context(), changes, nil)
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Equal(t, []*historyv1.Dot{{Actor: "other", Counter: 4}}, server.submits[0].Context)
	require.Equal(t, []*historyv1.Dot{{Actor: "other", Counter: 4}, {Actor: "replica", Counter: 1}}, server.submits[1].Context)
}

func TestSubmitRetainsFinalValueForEntryReplacement(t *testing.T) {
	server := &testServer{}
	history := newTestHistory(t, server)
	id := registry.NewID("test", "entry")
	_, err := history.SubmitChanges(t.Context(), registry.ChangeSet{{Kind: registry.EntryDelete, Entry: registry.Entry{ID: id, Kind: "old"}}, {Kind: registry.EntryCreate, Entry: registry.Entry{ID: id, Kind: "new"}}}, nil)
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.submits, 1)
	require.Len(t, server.submits[0].Mutations, 1)
	require.Equal(t, id.String(), server.submits[0].Mutations[0].EntryId)
	require.False(t, server.submits[0].Mutations[0].Deleted)
	require.NotEmpty(t, server.submits[0].Mutations[0].Value)
}

type monotonicReportServer struct {
	historyv1.UnimplementedHistoryServiceServer
	revision uint64
	lose     bool
}

func (s *monotonicReportServer) ReportApplied(_ context.Context, request *historyv1.AppliedRequest) (*historyv1.Empty, error) {
	if request.Revision < s.revision {
		return nil, status.Error(codes.Aborted, "causal state changed")
	}
	s.revision = request.Revision
	if s.lose {
		s.lose = false
		return nil, status.Error(codes.Unavailable, "response lost")
	}
	return &historyv1.Empty{}, nil
}

func TestAppliedReportsDoNotMoveBackwards(t *testing.T) {
	for _, lose := range []bool{false, true} {
		t.Run(fmt.Sprint(lose), func(t *testing.T) {
			server := &monotonicReportServer{lose: lose}
			history := newTestHistory(t, server)
			err := history.ReportApplied(t.Context(), &registry.PublishedState{Version: version.New(3)}, nil)
			if lose {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, history.ReportApplied(t.Context(), &registry.PublishedState{Version: version.New(2)}, nil))
			err = history.FollowPublished(t.Context(), 3, func(*registry.PublishedState) error { return nil })
			require.Equal(t, codes.Unimplemented, status.Code(err))
		})
	}
}

func BenchmarkRemoteStoredVersion(b *testing.B) {
	history := newTestHistory(b, &testServer{})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := history.readVersion(b.Context(), 3, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRemoteAppliedReport(b *testing.B) {
	history := newTestHistory(b, &testServer{})
	published := &registry.PublishedState{Version: version.New(3)}
	b.ReportAllocs()
	for b.Loop() {
		if err := history.ReportApplied(b.Context(), published, nil); err != nil {
			b.Fatal(err)
		}
	}
}
