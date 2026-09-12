// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	apierror "github.com/wippyai/runtime/api/error"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
)

func snapshotCreate(name string) regapi.ChangeSet {
	return regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: regapi.Entry{
		ID: regapi.NewID("test", name), Kind: regapi.EntryKind,
	}}}
}

func requireSnapshotConflict(t *testing.T, err error) {
	t.Helper()
	var typed apierror.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, apierror.Conflict, typed.Kind())
}

func TestApplyAtRejectsChangedDurableAndOverlayState(t *testing.T) {
	for _, overlay := range []bool{false, true} {
		name := "durable"
		if overlay {
			name = "overlay"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reg, history, runner := newOverlayTestRegistryWithRunner(t)
			require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
			before := reg.Snapshot()
			require.NotZero(t, before.Revision)
			if overlay {
				_, err := reg.ApplyOverlay(ctx, "test:owner", 0, snapshotCreate("intervening"))
				require.NoError(t, err)
				require.Equal(t, before.Version.ID(), reg.Snapshot().Version.ID())
			} else {
				_, err := reg.Apply(ctx, snapshotCreate("intervening"))
				require.NoError(t, err)
			}
			current := reg.Snapshot()
			head, err := history.Head()
			require.NoError(t, err)
			// A rejected stale write must never reach the transition runner.
			transitions := runner.TransitionCount()
			_, err = reg.ApplyAt(ctx, before.Revision, snapshotCreate("stale"))
			requireSnapshotConflict(t, err)
			require.Equal(t, transitions, runner.TransitionCount())
			require.Equal(t, current, reg.Snapshot())
			afterHead, err := history.Head()
			require.NoError(t, err)
			require.Equal(t, head.ID(), afterHead.ID())
			_, err = reg.GetEntry(regapi.NewID("test", "stale"))
			require.Error(t, err)
		})
	}
}

func TestApplyAtSameSnapshotHasOneWinner(t *testing.T) {
	reg, history := newOverlayTestRegistry(t)
	base := reg.Snapshot()
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var workers sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := reg.ApplyAt(context.Background(), base.Revision, snapshotCreate(name))
			errorsOut <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errorsOut)
	winners := 0
	for err := range errorsOut {
		if err == nil {
			winners++
		} else {
			requireSnapshotConflict(t, err)
		}
	}
	require.Equal(t, 1, winners)
	require.Len(t, reg.Snapshot().Entries, 1)
	head, err := history.Head()
	require.NoError(t, err)
	require.Equal(t, uint(1), head.ID())
}

func TestApplyAtRejectsRevisionAfterHistoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	reg, _ := newOverlayTestRegistry(t)
	v1, err := reg.Apply(ctx, snapshotCreate("first"))
	require.NoError(t, err)
	base := reg.Snapshot()
	_, err = reg.Apply(ctx, snapshotCreate("second"))
	require.NoError(t, err)
	require.NoError(t, reg.ApplyVersion(ctx, v1))
	require.Equal(t, base.Version.ID(), reg.Snapshot().Version.ID())
	_, err = reg.ApplyAt(ctx, base.Revision, snapshotCreate("stale"))
	requireSnapshotConflict(t, err)
	_, err = reg.ApplyAt(ctx, reg.Snapshot().Revision, snapshotCreate("fresh"))
	require.NoError(t, err)
}

func TestApplyAtRejectsMissingRevision(t *testing.T) {
	reg, _ := newOverlayTestRegistry(t)
	before := reg.Snapshot()
	_, err := reg.ApplyAt(context.Background(), 0, snapshotCreate("missing"))
	requireSnapshotConflict(t, err)
	require.Equal(t, before, reg.Snapshot())
}
