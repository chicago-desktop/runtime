// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	regapi "github.com/wippyai/runtime/api/registry"
	regsystem "github.com/wippyai/runtime/system/registry"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type luaPreviewEffect struct{ target string }

func (e *luaPreviewEffect) PreviewDigest() (string, error) {
	sum := sha256.Sum256([]byte(e.target))
	return hex.EncodeToString(sum[:]), nil
}
func (*luaPreviewEffect) Prepare(context.Context) error  { return nil }
func (*luaPreviewEffect) Commit(context.Context) error   { return nil }
func (*luaPreviewEffect) Rollback(context.Context) error { return nil }

type luaPreviewDirective struct{}

func (luaPreviewDirective) Expand(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
	derived := regapi.Entry{ID: regapi.NewID("resolved", "service"), Kind: "registry.entry",
		Registry: regapi.EntryMetadata{Owner: "acme/example"}}
	resolution := (&regapi.DependencyResolution{
		InputDigest: "sha256:lua-preview-input",
		Roots:       []regapi.DependencyRoot{{ID: op.Entry.ID.String(), Component: "acme/example", Version: "1.0.0"}},
		Modules:     []regapi.ResolvedModule{{Name: "acme/example", Version: "1.0.0", Digest: "sha256:lua-preview-module"}},
	}).Canonical()
	return regapi.DirectiveResult{Applied: true, Resolution: resolution,
		Additional: []regapi.ScopedOperation{{Operation: regapi.Operation{Kind: regapi.EntryCreate, Entry: derived}, Scope: regapi.ScopeBaseline}},
		Effects:    []regapi.Effect{&luaPreviewEffect{target: "acme/example@1.0.0"}}}, nil
}

func newLuaPreviewRegistry() *regsystem.Reg {
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	return regsystem.NewRegistry(historymem.New(), snapshotRunner{builder}, builder, resolver, zap.NewNop(),
		regsystem.WithKindDirective("ns.dependency", luaPreviewDirective{}))
}

func TestChangesPreviewReturnsMeasuredExpandedOwnershipAndGuardsApply(t *testing.T) {
	reg := newLuaPreviewRegistry()
	runSnapshotApplyLua(setupContextWithTranscoder(), t, reg, `
		local changes = registry.snapshot():changes()
		changes:create({id="app:dependency", kind="ns.dependency", data={component="acme/example", version="1.0.0"}})
		local preview, preview_err = changes:preview()
		assert(preview_err == nil, tostring(preview_err))
		assert(#preview.digest == 64)
		assert(#preview.changes == 2)
		assert(#preview.history == 1)
		assert(preview.resolution.modules[1].name == "acme/example")
		local found = false
		for _, operation in ipairs(preview.changes) do
			if operation.entry.id == "resolved:service" then
				found = operation.entry.registry.owner == "acme/example"
			end
		end
		assert(found, "expanded ownership was not returned")
		assert(changes:apply())
		assert(registry.get("resolved:service") ~= nil)

		local invalidated = registry.snapshot():changes()
		invalidated:create({id="app:second", kind="ns.dependency", data={component="acme/example", version="1.0.0"}})
		assert(invalidated:preview())
		invalidated:create({id="app:changed", kind="registry.entry"})
		local result, apply_err = invalidated:apply()
		assert(result == nil and apply_err:kind() == errors.INVALID)
	`)
}

func TestChangesPreviewRequiresPermissionAndCurrentDurableSnapshot(t *testing.T) {
	reg := newLuaPreviewRegistry()
	ctx, release := strictOverlayContext(t)
	defer release()
	runSnapshotApplyLua(ctx, t, reg, `
		local changes = registry.snapshot():changes()
		changes:create({id="app:dependency", kind="ns.dependency", data={component="acme/example", version="1.0.0"}})
		local denied, denied_err = changes:preview()
		assert(denied == nil and denied_err:kind() == errors.PERMISSION_DENIED)
	`)

	runSnapshotApplyLua(setupContextWithTranscoder(), t, reg, `
		local overlay = registry.overlay("test:owner"):changes()
		overlay:create({id="app:overlay", kind="registry.entry"})
		local result, err = overlay:preview()
		assert(result == nil and err:kind() == errors.INVALID)
	`)
	require.Empty(t, reg.Snapshot().Entries)
}
