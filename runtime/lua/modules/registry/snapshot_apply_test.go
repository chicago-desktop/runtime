// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	lua "github.com/wippyai/go-lua"
	regapi "github.com/wippyai/runtime/api/registry"
	regsystem "github.com/wippyai/runtime/system/registry"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type snapshotRunner struct{ builder *topology.StateBuilder }

func (r snapshotRunner) Transition(_ context.Context, state regapi.State, changes regapi.ChangeSet) (regapi.State, error) {
	working := topology.NewStateMap(state)
	for _, op := range changes {
		var err error
		working, err = r.builder.ApplyOperation(working, op)
		if err != nil {
			return state, err
		}
	}
	return topology.StateMapToSlice(working), nil
}

func newSnapshotRegistry() *regsystem.Reg {
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	return regsystem.NewRegistry(historymem.New(), snapshotRunner{builder}, builder, resolver, zap.NewNop())
}

func runSnapshotApplyLua(ctx context.Context, t *testing.T, reg regapi.Registry, source string) {
	t.Helper()
	l := lua.NewState()
	defer l.Close()
	l.SetContext(regapi.WithRegistry(ctx, reg))
	lua.OpenErrors(l)
	setupModule(l)
	require.NoError(t, l.DoString(source))
}

func TestSnapshotApplyFencesDurableAndOverlayChanges(t *testing.T) {
	for _, mutate := range []string{
		`local other = registry.snapshot():changes(); other:create({id="test:other", kind="registry.entry"}); assert(other:apply())`,
		`local other = registry.overlay("test:owner"):changes(); other:create({id="test:other", kind="registry.entry"}); assert(other:apply())`,
	} {
		reg := newSnapshotRegistry()
		runSnapshotApplyLua(setupContextWithTranscoder(), t, reg, `
			local base = registry.snapshot()
			local changes = base:changes()
			changes:create({id="test:mine", kind="registry.entry"})
		`+mutate+`
			local version = registry.current_version():id()
			local result, err = changes:apply()
			assert(result == nil and err:kind() == errors.CONFLICT, tostring(err))
			assert(err:retryable() == true)
			assert(registry.current_version():id() == version)
			assert(registry.get("test:mine") == nil)
			local fresh = registry.snapshot():changes()
			fresh:create({id="test:mine", kind="registry.entry"})
			assert(fresh:apply())
			assert(registry.get("test:mine") ~= nil)
		`)
	}
}

// A backend without the optional capability must never receive an unguarded
// fallback write, even if it happens to expose a nonzero snapshot revision.
type unguardedSnapshotRegistry struct{ regapi.Registry }

func (r unguardedSnapshotRegistry) Apply(context.Context, regapi.ChangeSet) (regapi.Version, error) {
	panic("unguarded fallback write")
}

func TestSnapshotApplyRequiresGuardedBackend(t *testing.T) {
	reg := unguardedSnapshotRegistry{newSnapshotRegistry()}
	runSnapshotApplyLua(setupContextWithTranscoder(), t, reg, `
		local changes = registry.snapshot():changes()
		changes:create({id="test:mine", kind="registry.entry"})
		local result, err = changes:apply()
		assert(result == nil and err:kind() == errors.INVALID)
		assert(err:retryable() == false)
	`)
	require.Empty(t, reg.Snapshot().Entries)
}

func TestSnapshotApplyRequiresCurrentCaptureAndWritePermission(t *testing.T) {
	reg := newSnapshotRegistry()
	runSnapshotApplyLua(setupContextWithTranscoder(), t, reg, `
		local changes = registry.snapshot():changes()
		changes:create({id="test:first", kind="registry.entry"})
		local version = assert(changes:apply())
		local historic = assert(registry.snapshot_at(version:id())):changes()
		historic:create({id="test:historic", kind="registry.entry"})
		local result, err = historic:apply()
		assert(result == nil and err:kind() == errors.INVALID)
	`)
	before := reg.Snapshot()
	ctx, release := strictOverlayContext(t)
	defer release()
	runSnapshotApplyLua(ctx, t, reg, `
		local changes = registry.snapshot():changes()
		changes:create({id="test:denied", kind="registry.entry"})
		local result, err = changes:apply()
		assert(result == nil and err:kind() == errors.PERMISSION_DENIED)
	`)
	require.Equal(t, before, reg.Snapshot())
}
