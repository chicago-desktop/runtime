// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierror "github.com/wippyai/runtime/api/error"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type failOnceSnapshotHistory struct {
	*historymem.Storage
	failed bool
}

func (h *failOnceSnapshotHistory) Save(v regapi.Version, changes regapi.ChangeSet, head bool) error {
	if !h.failed {
		h.failed = true
		return errors.New("injected history failure")
	}
	return h.Storage.Save(v, changes, head)
}

func recoveryRegistry(history regapi.History, runner regapi.Runner) *Reg {
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	return NewRegistry(history, runner, builder, resolver, zap.NewNop())
}

func recoveryCreate(name string) regapi.ChangeSet {
	return regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: regapi.Entry{
		ID: regapi.NewID("test", name), Kind: regapi.EntryKind,
	}}}
}

func requireRecoveryConflict(t *testing.T, err error) {
	t.Helper()
	var typed apierror.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, apierror.Conflict, typed.Kind())
}

func TestApplyAtRejectsSnapshotBeforeLoadState(t *testing.T) {
	ctx := context.Background()
	reg, _, runner := newOverlayTestRegistryWithRunner(t)
	before := reg.Snapshot()
	require.NotZero(t, before.Revision)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	current := reg.Snapshot()
	require.NotEqual(t, before.Revision, current.Revision)
	transitions := runner.TransitionCount()

	_, err := reg.ApplyAt(ctx, before.Revision, recoveryCreate("stale-load"))
	requireRecoveryConflict(t, err)
	require.Equal(t, transitions, runner.TransitionCount())

	_, err = reg.ApplyAt(ctx, current.Revision, recoveryCreate("fresh-load"))
	require.NoError(t, err)
}

func TestApplyAtRejectsSnapshotAfterPartialFailedRollback(t *testing.T) {
	ctx := context.Background()
	history := NewErrorHistory()
	runner := NewMockRunner()
	reg := recoveryRegistry(history, runner)
	before := reg.Snapshot()
	partial := regapi.Entry{ID: regapi.NewID("runtime", "partial"), Kind: regapi.EntryKind}
	calls := 0
	runner.RunFunc = func(_ regapi.State, _ regapi.ChangeSet) (regapi.State, error) {
		calls++
		if calls == 1 {
			return regapi.State{recoveryCreate("failed")[0].Entry}, nil
		}
		return regapi.State{partial}, errors.New("injected rollback failure")
	}

	_, err := reg.Apply(ctx, recoveryCreate("failed"))
	require.Error(t, err)
	current := reg.Snapshot()
	require.NotEqual(t, before.Revision, current.Revision)
	require.Len(t, current.Entries, 1)
	require.Equal(t, partial.ID, current.Entries[0].ID)
	transitions := calls

	_, err = reg.ApplyAt(ctx, before.Revision, recoveryCreate("stale-partial"))
	requireRecoveryConflict(t, err)
	require.Equal(t, transitions, calls)
}

func TestApplyAtKeepsSnapshotUsableAfterSuccessfulRollback(t *testing.T) {
	ctx := context.Background()
	history := &failOnceSnapshotHistory{Storage: historymem.New()}
	builder := topology.NewStateBuilder(zap.NewNop(), topology.NewResolver())
	runner := newApplyingMockRunner(builder)
	reg := recoveryRegistry(history, runner)
	before := reg.Snapshot()

	_, err := reg.Apply(ctx, recoveryCreate("rolled-back"))
	require.Error(t, err)
	require.Equal(t, before, reg.Snapshot())

	_, err = reg.ApplyAt(ctx, before.Revision, recoveryCreate("accepted"))
	require.NoError(t, err)
	_, err = reg.GetEntry(regapi.NewID("test", "accepted"))
	require.NoError(t, err)
}
