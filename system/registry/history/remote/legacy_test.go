package remote

import (
	"context"
	"testing"

	"github.com/hashicorp/go-msgpack/v2/codec"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type legacyServer struct {
	historyv1.UnimplementedHistoryServiceServer
	versions  map[uint64]*historyv1.Version
	calls     int
	rootReads int
}

func (s *legacyServer) ListVersions(_ context.Context, req *historyv1.ReadRequest) (*historyv1.VersionList, error) {
	s.calls++
	if req.AfterRevision == 0 {
		return &historyv1.VersionList{Versions: []*historyv1.VersionMeta{{Revision: 0}, {Revision: 1}}, NextRevision: 1, HasMore: true}, nil
	}
	return &historyv1.VersionList{Versions: []*historyv1.VersionMeta{{Revision: 2, ParentRevision: 1}, {Revision: 3, ParentRevision: 1}}}, nil
}
func (s *legacyServer) GetVersion(_ context.Context, req *historyv1.GetRequest) (*historyv1.Version, error) {
	if !req.Exact && req.Revision == 0 {
		return s.versions[3], nil
	}
	if req.Revision == 0 {
		s.rootReads++
	}
	return s.versions[req.Revision], nil
}
func TestVersionsRebuildsBranchesWithPagedMetadata(t *testing.T) {
	server := &legacyServer{}
	history := newTestHistory(t, server)
	versions, err := history.Versions()
	require.NoError(t, err)
	require.Len(t, versions, 4)
	require.Equal(t, uint(1), versions[2].Previous().ID())
	require.Equal(t, uint(1), versions[3].Previous().ID())
	require.Equal(t, uint(2), versions[1].Next().ID())
	require.Equal(t, 2, server.calls)
}

func TestSnapshotReplayUsesExactRootAndLiveEntries(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Data: payload.NewString("root")}
	encoded, err := entryencoding.EncodeEntry(entry)
	require.NoError(t, err)
	server := &legacyServer{versions: map[uint64]*historyv1.Version{0: {Revision: 0, Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Value: encoded}}}, 3: {Revision: 3, Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Deleted: true}}}}}
	history := newTestHistory(t, server)
	builder := topology.NewStateBuilder(zap.NewNop(), nil)
	root, err := builder.BuildState(history, version.New(0))
	require.NoError(t, err)
	require.Equal(t, registry.State{entry}, root)
	latest, err := builder.BuildState(history, version.New(3))
	require.NoError(t, err)
	require.Empty(t, latest)
}

func TestLegacyGetRestoresOriginalEntry(t *testing.T) {
	original := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Data: payload.NewString("old")}
	updated := original
	updated.Data = payload.NewString("new")
	oldData, err := entryencoding.EncodeEntry(original)
	require.NoError(t, err)
	newData, err := entryencoding.EncodeEntry(updated)
	require.NoError(t, err)
	server := &legacyServer{versions: map[uint64]*historyv1.Version{1: {Revision: 1, Entries: []*historyv1.Mutation{{EntryId: original.ID.String(), Value: oldData}}}, 3: {Revision: 3, ParentRevision: 1, Entries: []*historyv1.Mutation{{EntryId: original.ID.String(), Value: newData}}}}}
	history := newTestHistory(t, server)
	changes, err := history.Get(version.New(3))
	require.NoError(t, err)
	require.Equal(t, registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: updated, OriginalEntry: &original}}, changes)
}

func TestImportedGetPreservesNoOpsOrderAndReleasedMetadata(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Registry: registry.EntryMetadata{Owner: "test/module", Root: true}}
	encoded, err := entryencoding.EncodeEntry(entry)
	require.NoError(t, err)
	var changeset []byte
	records := []map[string]any{
		{"Kind": registry.EntryUpdate, "Entry": map[string]any{"ID": entry.ID, "Kind": entry.Kind}},
		{"Kind": registry.EntryUpdate, "Entry": map[string]any{"ID": entry.ID, "Kind": entry.Kind}, "Current": map[string]any{"Module": "test/module", "Root": true}},
		{"Kind": registry.EntryDelete, "Entry": map[string]any{"ID": entry.ID, "Kind": entry.Kind}},
	}
	require.NoError(t, codec.NewEncoderBytes(&changeset, &codec.MsgpackHandle{}).Encode(records))
	server := &legacyServer{versions: map[uint64]*historyv1.Version{
		0: {Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Value: encoded}}},
		1: {Revision: 1, Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Value: encoded}}},
		3: {Revision: 3, ParentRevision: 1, LegacyChangeset: changeset, Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Deleted: true}}},
	}}
	history := newTestHistory(t, server)
	for range 2 {
		changes, err := history.GetContext(t.Context(), version.New(3))
		require.NoError(t, err)
		require.Len(t, changes, 3)
		require.Equal(t, registry.EntryUpdate, changes[0].Kind)
		require.Equal(t, registry.EntryUpdate, changes[1].Kind)
		require.Equal(t, registry.EntryDelete, changes[2].Kind)
		for _, operation := range changes {
			require.Equal(t, entry.Registry, operation.Entry.Registry)
			require.Equal(t, &entry, operation.OriginalEntry)
		}
	}
	require.Equal(t, 1, server.rootReads)
}

type nativeRootServer struct {
	historyv1.UnimplementedHistoryServiceServer
	head    *historyv1.Version
	failure error
}

func (s *nativeRootServer) GetVersion(_ context.Context, request *historyv1.GetRequest) (*historyv1.Version, error) {
	if request.Exact && request.Revision == 0 {
		return nil, status.Error(codes.NotFound, "history was not found")
	}
	if s.failure != nil {
		return nil, s.failure
	}
	if request.Revision != 0 && request.Revision != s.head.GetRevision() {
		return nil, status.Error(codes.NotFound, "history was not found")
	}
	return s.head, nil
}

func TestNativeRootHasNoDependencyResolution(t *testing.T) {
	history := newTestHistory(t, &nativeRootServer{head: &historyv1.Version{}})
	head, err := history.Head()
	require.NoError(t, err)
	_, err = history.GetDependencyResolution(head)
	require.ErrorIs(t, err, registry.ErrDependencyResolutionNotFound)
}

func TestNativeFirstVersionChanges(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	data, err := entryencoding.EncodeEntry(entry)
	require.NoError(t, err)
	history := newTestHistory(t, &nativeRootServer{head: &historyv1.Version{Revision: 1, Entries: []*historyv1.Mutation{{EntryId: entry.ID.String(), Value: data}}}})
	changes, err := history.GetContext(t.Context(), version.New(1))
	require.NoError(t, err)
	require.Equal(t, registry.ChangeSet{{Kind: registry.EntryCreate, Entry: entry}}, changes)
	_, err = history.GetContext(t.Context(), version.New(2))
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestMissingRootPreservesRegistryErrors(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.AlreadyExists, codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			history := newTestHistory(t, &nativeRootServer{failure: status.Error(code, "registry unavailable")})
			_, err := history.GetDependencyResolution(version.New(0))
			require.Equal(t, code, status.Code(err))
		})
	}
}
