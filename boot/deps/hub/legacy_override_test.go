package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	"github.com/wippyai/runtime/api/registry/history/migration"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	registryimpl "github.com/wippyai/runtime/system/registry"
	"github.com/wippyai/runtime/system/registry/expansion"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"github.com/wippyai/wapp"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestLegacyModuleTransitionReplacesEarlierAuthoredValue(t *testing.T) {
	for _, action := range []string{"update", "upgrade", "reinstall"} {
		t.Run(action, func(t *testing.T) {
			ctx := newTestContext()
			serviceID := regapi.NewID("arbitrary.namespace", "entry")
			artifacts := make(map[string][]byte)
			digests := make(map[string]string)
			for _, selected := range []string{"1.0.0", "2.0.0"} {
				data := buildWappBytes(t, []wapp.Entry{{ID: wapp.NewID(serviceID.NS, serviceID.Name), Kind: regapi.EntryKind, Data: map[string]any{"value": selected}}})
				sum := sha256.Sum256(data)
				artifacts[selected] = data
				digests[selected] = "sha256:" + hex.EncodeToString(sum[:])
			}
			client := &fakeHub{
				getManifest: func(_ context.Context, org, name, selected string) (*ModuleManifest, error) {
					return &ModuleManifest{Org: org, Name: name, Version: selected, VersionID: selected, Digest: digests[selected], SizeBytes: uint64(len(artifacts[selected])), URL: "memory://" + selected}, nil
				},
				getDownload: func(_ context.Context, params *DownloadParams) (*DownloadInfo, error) {
					return &DownloadInfo{URL: "memory://" + params.Version, Digest: digests[params.Version], Size: uint64(len(artifacts[params.Version]))}, nil
				},
				downloadFile: func(_ context.Context, url, destination string) error {
					return os.WriteFile(destination, artifacts[strings.TrimPrefix(url, "memory://")], 0600)
				},
			}
			resolver := topology.NewResolver()
			handler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: client, Logger: zap.NewNop(), Resolver: resolver, VendorDir: t.TempDir()})
			require.NoError(t, err)
			history := historymem.New()
			reg := registryimpl.NewRegistry(history, &bootRecordingRunner{}, topology.NewStateBuilder(zap.NewNop(), resolver), resolver, zap.NewNop(), registryimpl.WithKindDirective(regapi.NamespaceDependency, expansion.NewDependencyDirective(handler.Expand).WithResolutionTransition(handler.ReconcileResolution).WithChangesExpansion(handler.ExpandChanges)))
			ctx = regapi.WithRegistry(ctx, reg)
			require.NoError(t, reg.LoadState(ctx, nil, version.New(0)))
			root := regapi.Entry{ID: regapi.NewID("host", "root"), Kind: regapi.NamespaceDependency, Data: payload.New(map[string]any{"component": "acme/worker", "version": "1.0.0"})}
			_, err = reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: root}})
			require.NoError(t, err)
			service, err := reg.GetEntry(serviceID)
			require.NoError(t, err)
			require.Equal(t, "acme/worker", service.Registry.Owner)
			service.Data = payload.New(map[string]any{"value": "authored"})
			authoredVersion, err := reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: service}})
			require.NoError(t, err)
			authored, err := history.Get(authoredVersion)
			require.NoError(t, err)
			require.Len(t, authored, 1)
			require.Equal(t, serviceID, authored[0].Entry.ID)
			require.Equal(t, "acme/worker", authored[0].Entry.Registry.Owner)
			selected := "1.0.0"
			kind := regapi.EntryUpdate
			if action == "upgrade" {
				selected = "2.0.0"
			}
			if action == "reinstall" {
				_, err = reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryDelete, Entry: root}})
				require.NoError(t, err)
				_, err = reg.GetEntry(serviceID)
				require.Error(t, err)
				kind = regapi.EntryCreate
			}
			root.Data = payload.New(map[string]any{"component": "acme/worker", "version": selected})
			changedVersion, err := reg.Apply(ctx, regapi.ChangeSet{{Kind: kind, Entry: root}})
			require.NoError(t, err)
			changed, err := reg.GetEntry(serviceID)
			require.NoError(t, err)
			require.Equal(t, map[string]any{"value": selected}, changed.Data.Data())
			persisted, err := history.Get(changedVersion)
			require.NoError(t, err)
			for _, operation := range persisted {
				require.NotEqual(t, serviceID, operation.Entry.ID)
			}

			client.getManifest = func(context.Context, string, string, string) (*ModuleManifest, error) {
				t.Fatal("recorded replay selected a module version")
				return nil, nil
			}
			replayContext := newTestContext()
			replayHandler, err := NewDependencyHandler(DependencyHandlerOptions{Hub: client, Logger: zap.NewNop(), Resolver: resolver, VendorDir: t.TempDir()})
			require.NoError(t, err)
			versions, err := history.Versions()
			require.NoError(t, err)
			var replayState regapi.State
			var previous *regapi.DependencyResolution
			for _, stored := range versions[1:] {
				changes, err := history.Get(stored)
				require.NoError(t, err)
				target, err := history.GetDependencyResolution(stored)
				require.NoError(t, err)
				directive := expansion.NewDependencyDirective(func(ctx context.Context, operation regapi.Operation, state regapi.State) (regapi.DirectiveResult, error) {
					return replayHandler.ExpandRecordedChanges(ctx, regapi.ChangeSet{operation}, state, previous, target)
				}).WithChangesExpansion(func(ctx context.Context, changes regapi.ChangeSet, state regapi.State) (regapi.DirectiveResult, error) {
					return replayHandler.ExpandRecordedChanges(ctx, changes, state, previous, target)
				})
				planner := expansion.NewPlanner(map[regapi.Kind][]regapi.Directive{regapi.NamespaceDependency: {directive}}, resolver, zap.NewNop())
				plan, err := planner.Expand(replayContext, changes, replayState)
				require.NoError(t, err)
				plan.Ops, err = planner.SortOps(replayState, plan.Ops)
				require.NoError(t, err)
				_, err = planner.PrepareEffects(replayContext, plan.Effects)
				require.NoError(t, err)
				operations, _ := plan.SplitScopes()
				replayState, err = (&bootRecordingRunner{}).Transition(replayContext, replayState, operations)
				require.NoError(t, err)
				require.NoError(t, planner.CommitEffects(replayContext, plan.Effects))
				require.NoError(t, planner.FinalizeEffects(replayContext, plan.Effects))
				previous = target
			}
			require.ElementsMatch(t, reg.Snapshot().Entries, replayState)
			var rawBundle, materialized bytes.Buffer
			baselineSum := sha256.Sum256(nil)
			writer, err := migration.NewWriter(&rawBundle, migration.Header{Format: migration.RawFormat, BaselineDigest: baselineSum[:], Manifest: migration.Manifest{Head: uint64(changedVersion.ID()), Maximum: uint64(changedVersion.ID()) + 1, Count: uint64(len(versions)) + 1}}, 1024*1024)
			require.NoError(t, err)
			for _, stored := range versions {
				record := &migration.Record{Revision: uint64(stored.ID())}
				if parent := stored.Previous(); parent != nil {
					record.Parent = uint64(parent.ID())
				}
				if stored.ID() != 0 {
					operations, err := history.Get(stored)
					require.NoError(t, err)
					record.Changes = encodeRecordedOperations(t, operations)
					resolution, err := history.GetDependencyResolution(stored)
					require.NoError(t, err)
					if resolution != nil {
						record.Resolution, err = json.Marshal(resolution)
						require.NoError(t, err)
						record.ResolutionDigest = &resolution.Digest
					}
				}
				require.NoError(t, writer.Write(record))
			}
			branchChanges, err := history.Get(versions[1])
			require.NoError(t, err)
			branchChanges[0].Kind = regapi.EntryUpdate
			branchGraph, err := history.GetDependencyResolution(versions[1])
			require.NoError(t, err)
			branchGraphBytes, err := json.Marshal(branchGraph)
			require.NoError(t, err)
			require.NoError(t, writer.Write(&migration.Record{Revision: uint64(changedVersion.ID()) + 1, Parent: uint64(authoredVersion.ID()), Changes: encodeRecordedOperations(t, branchChanges), Resolution: branchGraphBytes, ResolutionDigest: &branchGraph.Digest}))
			require.NoError(t, writer.Close())
			require.NoError(t, MaterializeHistory(context.Background(), &rawBundle, &materialized, MaterializeHistoryOptions{Hub: client, ScratchDir: t.TempDir(), MaximumRecordBytes: 1024 * 1024}))
			reader, err := migration.NewReader(&materialized, 1024*1024)
			require.NoError(t, err)
			var finalState regapi.State
			for {
				record, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				snapshot := new(historyv1.Version)
				require.NoError(t, proto.Unmarshal(record.Snapshot, snapshot))
				decodedState, _, err := decodeMaterializedState(snapshot)
				require.NoError(t, err)
				if record.Revision == uint64(changedVersion.ID()) {
					finalState = decodedState
				}
				if record.Revision > uint64(changedVersion.ID()) {
					require.Len(t, decodedState, 2)
					for _, entry := range decodedState {
						if entry.ID == serviceID {
							require.Equal(t, map[string]any{"value": "1.0.0"}, entry.Data.Data())
						}
					}
				}
			}
			require.ElementsMatch(t, reg.Snapshot().Entries, finalState)
			firstChanges, err := history.Get(versions[1])
			require.NoError(t, err)
			firstGraph, err := history.GetDependencyResolution(versions[1])
			require.NoError(t, err)
			rootValue, err := entryencoding.EncodeEntry(firstChanges[0].Entry)
			require.NoError(t, err)
			graphBytes, err := json.Marshal(firstGraph)
			require.NoError(t, err)
			baselineBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&historyv1.Version{Entries: []*historyv1.Mutation{{EntryId: firstChanges[0].Entry.ID.String(), Value: rootValue}}, Resolution: graphBytes})
			require.NoError(t, err)
			rootSum := sha256.Sum256(baselineBytes)
			rawBundle.Reset()
			materialized.Reset()
			writer, err = migration.NewWriter(&rawBundle, migration.Header{Format: migration.RawFormat, Baseline: baselineBytes, BaselineDigest: rootSum[:], Manifest: migration.Manifest{Count: 1}}, 1024*1024)
			require.NoError(t, err)
			require.NoError(t, writer.Write(&migration.Record{}))
			require.NoError(t, writer.Close())
			require.NoError(t, MaterializeHistory(context.Background(), &rawBundle, &materialized, MaterializeHistoryOptions{Hub: client, ScratchDir: t.TempDir(), MaximumRecordBytes: 1024 * 1024}))
			reader, err = migration.NewReader(&materialized, 1024*1024)
			require.NoError(t, err)
			materializedRoot, err := reader.Next()
			require.NoError(t, err)
			rootSnapshot := new(historyv1.Version)
			require.NoError(t, proto.Unmarshal(materializedRoot.Snapshot, rootSnapshot))
			rootState, _, err := decodeMaterializedState(rootSnapshot)
			require.NoError(t, err)
			require.Len(t, rootState, 2)
			for _, entry := range rootState {
				if entry.ID == serviceID {
					require.Equal(t, map[string]any{"value": "1.0.0"}, entry.Data.Data())
				}
			}
		})
	}
}
