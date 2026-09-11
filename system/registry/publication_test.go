package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type publicationHistory struct {
	*historymem.Storage
	follow        func(context.Context, func(*registry.PublishedState) error) error
	submitted     chan struct{}
	release       chan struct{}
	published     *registry.PublishedState
	failure       error
	reportFailure error
	reportErr     error
	mu            sync.Mutex
	restored      uint64
	reported      uint64
}

func (h *publicationHistory) SubmitChanges(context.Context, registry.ChangeSet, *registry.DependencyResolution) (*registry.HistoryReceipt, error) {
	close(h.submitted)
	return &registry.HistoryReceipt{RequestID: "request", Revision: 2, Status: "stored"}, nil
}
func (h *publicationHistory) AwaitPublished(ctx context.Context, _ *registry.HistoryReceipt) (*registry.PublishedState, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.release:
		return h.published, h.failure
	}
}
func (h *publicationHistory) ReadPublished(context.Context, uint64) (*registry.PublishedState, error) {
	return h.published, nil
}
func (h *publicationHistory) RestoreChanges(_ context.Context, target uint64) (*registry.HistoryReceipt, error) {
	h.restored = target
	return &registry.HistoryReceipt{RequestID: "restore", Revision: 3, Status: "published"}, nil
}
func (h *publicationHistory) FollowPublished(ctx context.Context, _ uint64, apply func(*registry.PublishedState) error) error {
	if h.follow != nil {
		return h.follow(ctx, apply)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (h *publicationHistory) ReportApplied(_ context.Context, published *registry.PublishedState, err error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reported = uint64(published.Version.ID())
	h.reportFailure = err
	return h.reportErr
}

func newPublicationRegistry(h *publicationHistory) *Reg {
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	return NewRegistry(h, newApplyingMockRunner(builder), builder, resolver, zap.NewNop())
}

func TestApplyWaitsForPublicationBeforeLocalEffects(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Data: payload.NewString("published")}
	h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(7), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: entry}}}}
	reg := newPublicationRegistry(h)
	result := make(chan error, 1)
	go func() {
		_, err := reg.Apply(context.Background(), registry.ChangeSet{{Kind: registry.EntryCreate, Entry: entry}})
		result <- err
	}()
	select {
	case <-h.submitted:
	case err := <-result:
		t.Fatalf("Apply bypassed publication: %v", err)
	case <-time.After(time.Second):
		t.Fatal("submit timed out")
	}
	require.Empty(t, reg.Snapshot().Entries)
	require.Zero(t, reg.Snapshot().Version.ID())
	close(h.release)
	require.NoError(t, <-result)
	require.Equal(t, uint(7), reg.Snapshot().Version.ID())
	require.Equal(t, registry.State{entry}, reg.Snapshot().Entries)
	require.Equal(t, uint64(7), h.reported)
}

func TestConflictPreservesCurrentState(t *testing.T) {
	h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), failure: registry.ErrHistoryConflict}
	close(h.release)
	reg := newPublicationRegistry(h)
	_, err := reg.Apply(context.Background(), registry.ChangeSet{{Kind: registry.EntryCreate, Entry: registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}}})
	require.ErrorIs(t, err, registry.ErrHistoryConflict)
	require.Empty(t, reg.Snapshot().Entries)
	require.Zero(t, reg.Snapshot().Version.ID())
}

func TestRemoteRestoreCreatesPublishedVersion(t *testing.T) {
	h := &publicationHistory{Storage: historymem.New(), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(8)}}
	close(h.release)
	reg := newPublicationRegistry(h)
	require.NoError(t, reg.ApplyVersion(context.Background(), version.New(2)))
	require.Equal(t, uint64(2), h.restored)
	require.Equal(t, uint(8), reg.Snapshot().Version.ID())
}

func TestRemoteLoadUsesOnlyPublishedEntries(t *testing.T) {
	baseline := registry.Entry{ID: registry.NewID("test", "baseline"), Kind: registry.EntryKind}
	deleted := registry.Entry{ID: registry.NewID("test", "deleted"), Kind: registry.EntryKind}
	authored := registry.Entry{ID: registry.NewID("test", "authored"), Kind: registry.EntryKind, Registry: registry.EntryMetadata{Owner: "owner"}}
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(5), Changes: registry.ChangeSet{{Kind: registry.EntryDelete, Entry: deleted}, {Kind: registry.EntryUpdate, Entry: authored}}}}
	reg := newPublicationRegistry(h)
	require.NoError(t, reg.LoadState(context.Background(), registry.State{baseline, deleted}, version.New(5)))
	require.ElementsMatch(t, registry.State{authored}, reg.Snapshot().Entries)
	require.Equal(t, uint64(5), h.reported)
	fresh := newPublicationRegistry(h)
	require.NoError(t, fresh.LoadState(t.Context(), nil, version.New(5)))
	require.ElementsMatch(t, reg.Snapshot().Entries, fresh.Snapshot().Entries)
	overlay := registry.Entry{ID: registry.NewID("test", "overlay"), Kind: registry.EntryKind}
	_, err := reg.ApplyOverlay(t.Context(), "test", 0, registry.ChangeSet{{Kind: registry.EntryCreate, Entry: overlay}})
	require.NoError(t, err)
	require.NoError(t, reg.applyPublication(t.Context(), h, &registry.PublishedState{Version: version.New(6)}, reg.baseline, false))
	require.Len(t, reg.Snapshot().Entries, 1)
	require.Equal(t, overlay.ID, reg.Snapshot().Entries[0].ID)
}

func TestPublishedTransitionFailureIsReported(t *testing.T) {
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(4), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}}}}}
	reg := newPublicationRegistry(h)
	runner := reg.runner.(*MockRunner)
	runner.RunFunc = nil
	runner.err = errors.New("handler failed")
	err := reg.LoadState(context.Background(), nil, version.New(4))
	require.Error(t, err)
	require.Error(t, h.reportFailure)
	require.Zero(t, reg.Snapshot().Version.ID())
}

func TestPublicationRejectsInvalidOperationBeforeSubmit(t *testing.T) {
	existing := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	changedKind := existing
	changedKind.Kind = "different"
	missing := registry.Entry{ID: registry.NewID("test", "missing"), Kind: registry.EntryKind}
	for _, operation := range []registry.Operation{{Kind: registry.EntryCreate, Entry: existing}, {Kind: registry.EntryUpdate, Entry: missing}, {Kind: registry.EntryUpdate, Entry: changedKind}, {Kind: registry.EntryDelete, Entry: missing}} {
		t.Run(operation.Kind+operation.Entry.ID.String()+operation.Entry.Kind, func(t *testing.T) {
			h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(1), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: existing}}}}
			close(h.release)
			reg := newPublicationRegistry(h)
			require.NoError(t, reg.LoadState(t.Context(), nil, version.New(1)))
			_, err := reg.Apply(t.Context(), registry.ChangeSet{operation})
			require.Error(t, err)
			select {
			case <-h.submitted:
				t.Fatal("invalid operation reached durable submission")
			default:
			}
			require.Equal(t, registry.State{existing}, reg.Snapshot().Entries)
		})
	}
}

func TestPublicationPermitsExplicitKindReplacement(t *testing.T) {
	original := registry.Entry{ID: registry.NewID("test", "entry"), Kind: "original"}
	replacement := original
	replacement.Kind = "replacement"
	h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(1), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: original}}}}
	close(h.release)
	reg := newPublicationRegistry(h)
	require.NoError(t, reg.LoadState(t.Context(), nil, version.New(1)))
	h.published = &registry.PublishedState{Version: version.New(2), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: replacement}}}
	_, err := reg.Apply(t.Context(), registry.ChangeSet{{Kind: registry.EntryDelete, Entry: original}, {Kind: registry.EntryCreate, Entry: replacement}})
	require.NoError(t, err)
	require.Equal(t, registry.State{replacement}, reg.Snapshot().Entries)
}

func TestBufferedPublicationCannotReportOlderThanLocalApply(t *testing.T) {
	h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(1)}}
	close(h.release)
	reg := newPublicationRegistry(h)
	require.NoError(t, reg.LoadState(t.Context(), nil, version.New(1)))
	ready := make(chan struct{})
	releaseWatch := make(chan struct{})
	h.follow = func(_ context.Context, apply func(*registry.PublishedState) error) error {
		close(ready)
		<-releaseWatch
		return apply(&registry.PublishedState{Version: version.New(2)})
	}
	done := make(chan error, 1)
	go func() { done <- reg.FollowPublications(t.Context()) }()
	<-ready
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	h.published = &registry.PublishedState{Version: version.New(3), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: entry}}}
	_, err := reg.Apply(t.Context(), registry.ChangeSet{{Kind: registry.EntryCreate, Entry: entry}})
	require.NoError(t, err)
	close(releaseWatch)
	require.NoError(t, <-done)
	require.Equal(t, uint64(3), h.reported)
	require.Equal(t, registry.State{entry}, reg.Snapshot().Entries)
}

func BenchmarkPublishedSnapshot(b *testing.B) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(1), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: entry}}}}
	reg := newPublicationRegistry(h)
	baseline := registry.State{entry}
	b.ReportAllocs()
	for b.Loop() {
		if err := reg.applyPublication(b.Context(), h, h.published, baseline, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPublicationPlan(b *testing.B) {
	root := registry.Entry{ID: registry.NewID("test", "root"), Kind: "test.expand"}
	derived := registry.Entry{ID: registry.NewID("test", "derived"), Kind: registry.EntryKind}
	h := &publicationHistory{Storage: historymem.New()}
	reg := newPublicationRegistry(h)
	reg.directivesByKind = map[registry.Kind][]registry.Directive{root.Kind: {directiveFunc(func(context.Context, registry.Operation, registry.State) (registry.DirectiveResult, error) {
		return registry.DirectiveResult{Applied: true, Additional: []registry.ScopedOperation{{Operation: registry.Operation{Kind: registry.EntryCreate, Entry: derived}, Scope: registry.ScopeBaseline}}}, nil
	})}}
	changes := registry.ChangeSet{{Kind: registry.EntryCreate, Entry: root}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := reg.planSubmission(b.Context(), changes, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRepeatedPublicationRetriesLostAppliedReport(t *testing.T) {
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(1)}, reportErr: errors.New("report response lost")}
	reg := newPublicationRegistry(h)
	require.Error(t, reg.LoadState(t.Context(), nil, version.New(1)))
	require.Equal(t, uint(1), reg.Snapshot().Version.ID())
	h.reportErr = nil
	h.reported = 0
	require.NoError(t, reg.applyPublication(t.Context(), h, h.published, nil, false))
	require.Equal(t, uint64(1), h.reported)
}

func TestRemoteLoadPreservesImportedRoot(t *testing.T) {
	imported := registry.Entry{ID: registry.NewID("test", "imported"), Kind: registry.EntryKind}
	local := registry.Entry{ID: registry.NewID("test", "local"), Kind: registry.EntryKind}
	h := &publicationHistory{Storage: historymem.New(), submitted: make(chan struct{}), release: make(chan struct{}), published: &registry.PublishedState{Version: version.New(0), Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: imported}}}}
	close(h.release)
	reg := newPublicationRegistry(h)
	require.NoError(t, reg.LoadState(t.Context(), registry.State{local}, version.New(0)))
	select {
	case <-h.submitted:
		t.Fatal("imported root triggered baseline submission")
	default:
	}
	require.Equal(t, registry.State{imported}, reg.Snapshot().Entries)
}

func BenchmarkPublishedResolution(b *testing.B) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	graph := (&registry.DependencyResolution{InputDigest: "test"}).Canonical()
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(1), Resolution: graph, Changes: registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: entry}}}}
	reg := newPublicationRegistry(h)
	reg.directivesByKind = map[registry.Kind][]registry.Directive{registry.NamespaceDependency: {hardeningDirective{reconcile: func(context.Context, registry.State, registry.State, *registry.DependencyResolution) (registry.DirectiveResult, error) {
		return registry.DirectiveResult{Applied: true, Resolution: graph, Additional: []registry.ScopedOperation{{Operation: registry.Operation{Kind: registry.EntryUpdate, Entry: entry}, Scope: registry.ScopeBaseline}}}, nil
	}}}}
	b.ReportAllocs()
	for b.Loop() {
		if err := reg.applyPublication(b.Context(), h, h.published, nil, true); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPublishedReconciliationPreservesExplicitDeletion(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "deleted"), Kind: registry.EntryKind}
	graph := (&registry.DependencyResolution{InputDigest: "test"}).Canonical()
	h := &publicationHistory{Storage: historymem.New(), published: &registry.PublishedState{Version: version.New(1), Resolution: graph, Changes: registry.ChangeSet{{Kind: registry.EntryDelete, Entry: entry}}}}
	reg := newPublicationRegistry(h)
	reg.directivesByKind = map[registry.Kind][]registry.Directive{registry.NamespaceDependency: {hardeningDirective{reconcile: func(context.Context, registry.State, registry.State, *registry.DependencyResolution) (registry.DirectiveResult, error) {
		return registry.DirectiveResult{Applied: true, Resolution: graph, Additional: []registry.ScopedOperation{{Operation: registry.Operation{Kind: registry.EntryCreate, Entry: entry}, Scope: registry.ScopeBaseline}}}, nil
	}}}}
	require.NoError(t, reg.LoadState(t.Context(), nil, version.New(1)))
	require.Empty(t, reg.Snapshot().Entries)
	require.NoError(t, h.reportFailure)
}
