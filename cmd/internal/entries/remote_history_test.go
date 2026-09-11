package entries

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/boot"
	regapi "github.com/wippyai/runtime/api/registry"
	bootpkg "github.com/wippyai/runtime/boot"
	"github.com/wippyai/runtime/internal/version"
	sysreg "github.com/wippyai/runtime/system/registry"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type recoveryHistory struct {
	*historymem.Storage
	published *regapi.PublishedState
}

func (h *recoveryHistory) Head() (regapi.Version, error) { return h.published.Version, nil }
func (h *recoveryHistory) ReadPublished(context.Context, uint64) (*regapi.PublishedState, error) {
	return h.published, nil
}
func (h *recoveryHistory) SubmitChanges(context.Context, regapi.ChangeSet, *regapi.DependencyResolution) (*regapi.HistoryReceipt, error) {
	panic("unexpected submit")
}
func (h *recoveryHistory) AwaitPublished(context.Context, *regapi.HistoryReceipt) (*regapi.PublishedState, error) {
	panic("unexpected await")
}
func (h *recoveryHistory) RestoreChanges(context.Context, uint64) (*regapi.HistoryReceipt, error) {
	panic("unexpected restore")
}
func (h *recoveryHistory) FollowPublished(context.Context, uint64, func(*regapi.PublishedState) error) error {
	panic("unexpected follow")
}
func (h *recoveryHistory) ReportApplied(context.Context, *regapi.PublishedState, error) error {
	return nil
}

type recoveryRunner struct{}

func (recoveryRunner) Transition(_ context.Context, state regapi.State, changes regapi.ChangeSet) (regapi.State, error) {
	result := topology.NewStateMap(state)
	for _, operation := range changes {
		if operation.Kind == regapi.EntryDelete {
			delete(result, operation.Entry.ID)
		} else {
			result[operation.Entry.ID] = operation.Entry
		}
	}
	return topology.StateMapToSlice(result), nil
}

func TestMissingLockAndSourcesRestoresRemoteHistory(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, err := bootpkg.NewBootstrapContext(zap.NewNop(), boot.NewConfig())
	require.NoError(t, err)
	entry := regapi.Entry{ID: regapi.NewID("test", "recovered"), Kind: regapi.EntryKind}
	history := &recoveryHistory{Storage: historymem.New(), published: &regapi.PublishedState{Version: version.New(4), Changes: regapi.ChangeSet{{Kind: regapi.EntryUpdate, Entry: entry}}}}
	resolver := topology.NewResolver()
	reg := sysreg.NewRegistry(history, recoveryRunner{}, topology.NewStateBuilder(zap.NewNop(), resolver), resolver, zap.NewNop())
	ctx = regapi.WithRegistry(ctx, reg)
	ctx = regapi.WithResolver(ctx, resolver)
	require.NoError(t, LoadFromLockFile(ctx, zap.NewNop()))
	require.Equal(t, regapi.State{entry}, reg.Snapshot().Entries)
	require.Equal(t, uint(4), reg.Snapshot().Version.ID())
}
