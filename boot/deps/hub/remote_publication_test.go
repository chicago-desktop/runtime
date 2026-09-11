package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	registryimpl "github.com/wippyai/runtime/system/registry"
	"github.com/wippyai/runtime/system/registry/expansion"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"github.com/wippyai/wapp"
	"go.uber.org/zap"
)

type publishedModuleHistory struct {
	*historymem.Storage
	published *regapi.PublishedState
}

func (h *publishedModuleHistory) SubmitChanges(_ context.Context, changes regapi.ChangeSet, resolution *regapi.DependencyResolution) (*regapi.HistoryReceipt, error) {
	state := make(regapi.StateMap)
	for _, op := range h.published.Changes {
		state[op.Entry.ID] = op.Entry
	}
	for _, op := range changes {
		if op.Kind == regapi.EntryDelete {
			delete(state, op.Entry.ID)
		} else {
			state[op.Entry.ID] = op.Entry
		}
	}
	h.published = &regapi.PublishedState{Version: version.New(h.published.Version.ID() + 1), Resolution: resolution}
	for _, entry := range state {
		h.published.Changes = append(h.published.Changes, regapi.Operation{Kind: regapi.EntryUpdate, Entry: entry})
	}
	return &regapi.HistoryReceipt{RequestID: "test", Status: "published"}, nil
}
func (h *publishedModuleHistory) AwaitPublished(context.Context, *regapi.HistoryReceipt) (*regapi.PublishedState, error) {
	return h.published, nil
}
func (h *publishedModuleHistory) ReadPublished(context.Context, uint64) (*regapi.PublishedState, error) {
	return h.published, nil
}
func (*publishedModuleHistory) RestoreChanges(context.Context, uint64) (*regapi.HistoryReceipt, error) {
	return nil, regapi.ErrHistoryOperationUnsupported
}
func (*publishedModuleHistory) FollowPublished(context.Context, uint64, func(*regapi.PublishedState) error) error {
	return regapi.ErrHistoryOperationUnsupported
}
func (*publishedModuleHistory) ReportApplied(context.Context, *regapi.PublishedState, error) error {
	return nil
}

func TestRemotePublicationStoresDerivedModuleEntries(t *testing.T) {
	testRemotePublicationModules(t, false, false)
}

func TestRemotePublicationRejectsIncompleteImportedSnapshot(t *testing.T) {
	testRemotePublicationModules(t, true, false)
}

func TestRemotePublicationPreservesAuthoredModuleValue(t *testing.T) {
	testRemotePublicationModules(t, false, true)
}

func testRemotePublicationModules(t *testing.T, legacy, authored bool) {
	t.Chdir(t.TempDir())
	artifact := buildWappBytes(t, []wapp.Entry{{ID: wapp.NewID("acme.worker", "service"), Kind: regapi.EntryKind}})
	sum := sha256.Sum256(artifact)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	downloads := 0
	manifests := 0
	hubClient := &fakeHub{
		getManifest: func(context.Context, string, string, string) (*ModuleManifest, error) {
			manifests++
			return &ModuleManifest{Org: "acme", Name: "worker", Version: "1.2.3", VersionID: "1.2.3", Digest: digest, SizeBytes: uint64(len(artifact)), URL: "memory://worker"}, nil
		},
		getDownload: func(context.Context, *DownloadParams) (*DownloadInfo, error) {
			return &DownloadInfo{URL: "memory://worker", Digest: digest, Size: uint64(len(artifact))}, nil
		},
		downloadFile: func(_ context.Context, _ string, destination string) error {
			downloads++
			return os.WriteFile(destination, artifact, 0600)
		},
	}
	history := &publishedModuleHistory{Storage: historymem.New(), published: &regapi.PublishedState{Version: version.New(0)}}
	newRegistry := func() (context.Context, *registryimpl.Reg) {
		ctx := newTestContext()
		resolver := topology.NewResolver()
		handler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: hubClient, Logger: zap.NewNop(), Resolver: resolver, VendorDir: t.TempDir()})
		require.NoError(t, err)
		reg := registryimpl.NewRegistry(history, &bootRecordingRunner{}, topology.NewStateBuilder(zap.NewNop(), resolver), resolver, zap.NewNop(), registryimpl.WithKindDirective(regapi.NamespaceDependency, expansion.NewDependencyDirective(handler.Expand).WithResolutionTransition(handler.ReconcileResolution).WithChangesExpansion(handler.ExpandChanges)))
		return regapi.WithRegistry(ctx, reg), reg
	}
	ctx, reg := newRegistry()
	require.NoError(t, reg.LoadState(ctx, nil, version.New(0)))
	root := regapi.Entry{ID: regapi.NewID("app", "worker"), Kind: regapi.NamespaceDependency, Data: payload.New(map[string]any{"component": "acme/worker", "version": "1.2.3"})}
	_, err := reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: root}})
	require.NoError(t, err)
	service, err := reg.GetEntry(regapi.NewID("acme.worker", "service"))
	require.NoError(t, err)
	require.Len(t, history.published.Changes, 2)
	if authored {
		service.Data = payload.NewString("authored")
		_, err := reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: service}})
		require.NoError(t, err)
	}
	if legacy {
		history.published.Changes = regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: root}}
		ctx, reg = newRegistry()
		require.ErrorContains(t, reg.LoadState(ctx, nil, history.published.Version), "published snapshot is missing derived entry")
		require.Empty(t, reg.Snapshot().Entries)
		require.Zero(t, reg.Snapshot().Version.ID())
		return
	}
	firstDownloads, firstManifests := downloads, manifests
	extra := regapi.Entry{ID: regapi.NewID("app", "extra"), Kind: regapi.EntryKind}
	_, err = reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: extra}})
	require.NoError(t, err)
	retained, err := reg.GetEntry(service.ID)
	require.NoError(t, err)
	require.Equal(t, service, retained)
	require.Equal(t, firstDownloads, downloads)
	require.Equal(t, firstManifests, manifests)
	storedGraph := history.published.Resolution.Digest
	freshCtx, fresh := newRegistry()
	require.NoError(t, fresh.LoadState(freshCtx, nil, history.published.Version))
	require.ElementsMatch(t, reg.Snapshot().Entries, fresh.Snapshot().Entries)
	require.Equal(t, storedGraph, fresh.Snapshot().Registry.Resolution.Digest)
	require.Equal(t, firstManifests, manifests)
}
