// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierror "github.com/wippyai/runtime/api/error"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
)

func requirePreviewApplyConflict(t *testing.T, err error) {
	t.Helper()
	var typed apierror.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, apierror.Conflict, typed.Kind())
}

func TestApplyPreviewAppliesUnchangedNoDirectivePreview(t *testing.T) {
	ctx := context.Background()
	reg, runner := previewRegistry(t, nil)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	base := reg.Snapshot()
	changes := regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: regapi.Entry{
		ID: regapi.NewID("app", "plain"), Kind: regapi.EntryKind,
		Data: payload.New(map[string]any{"value": "reviewed"}),
	}}}

	preview, err := reg.PreviewAt(ctx, base.Revision, changes)
	require.NoError(t, err)
	require.Len(t, preview.Digest, 64)
	transitions := runner.TransitionCount()

	applied, err := reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changes)
	require.NoError(t, err)
	require.Equal(t, uint(1), applied.ID())
	require.Equal(t, transitions+1, runner.TransitionCount())
	require.Equal(t, preview.Changes, clonePreviewChanges(runner.LastTransition()))
}

func TestApplyPreviewAppliesUnchangedDirectivePreviewAndIgnoresReturnedMutation(t *testing.T) {
	ctx := context.Background()
	var effects []*previewEffect
	derived := regapi.Entry{ID: regapi.NewID("acme.preview", "service"), Kind: regapi.EntryKind,
		Data: payload.New(map[string]any{"selection": "one"})}
	directive := directiveFunc(func(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		effect := &previewEffect{}
		effects = append(effects, effect)
		return regapi.DirectiveResult{
			Applied:    true,
			Resolution: previewResolution(op.Entry.ID),
			Additional: []regapi.ScopedOperation{{
				Operation: regapi.Operation{Kind: regapi.EntryCreate, Entry: derived}, Scope: regapi.ScopeBaseline,
			}},
			Effects: []regapi.Effect{effect},
		}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	changes := previewDependencyChange()
	preview, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, changes)
	require.NoError(t, err)
	require.Len(t, effects, 1)
	require.Equal(t, 1, effects[0].rollback)

	// The application displays this object, so it is intentionally hostile to
	// it. ApplyPreview must remeasure the live expansion, not trust these fields.
	findPreviewOperation(t, preview.Changes, derived.ID).Entry.Data.Data().(map[string]any)["selection"] = "tampered"
	preview.Resolution.Modules[0].Version = "tampered"
	transitions := runner.TransitionCount()

	_, err = reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changes)
	require.NoError(t, err)
	require.Equal(t, transitions+1, runner.TransitionCount())
	require.Len(t, effects, 2)
	require.Equal(t, 1, effects[1].prepare)
	require.Equal(t, 1, effects[1].commit)
	require.Equal(t, 1, effects[1].finalize)
	require.Zero(t, effects[1].rollback)
	actual := findPreviewOperation(t, runner.LastTransition(), derived.ID)
	require.Equal(t, "one", actual.Entry.Data.Data().(map[string]any)["selection"])
	require.Equal(t, "v1.0.0", reg.Snapshot().Registry.Resolution.Modules[0].Version)
}

func TestApplyPreviewRejectsChangedDerivedPayloadAndResolutionBeforeActivation(t *testing.T) {
	ctx := context.Background()
	selection := "one"
	var effects []*previewEffect
	directive := directiveFunc(func(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		effect := &previewEffect{}
		effects = append(effects, effect)
		derived := regapi.Entry{ID: regapi.NewID("acme.preview", "service"), Kind: regapi.EntryKind,
			Data: payload.New(map[string]any{"selection": selection})}
		resolution := previewResolution(op.Entry.ID)
		resolution.Modules[0].Version = "v" + selection
		resolution = resolution.Canonical()
		return regapi.DirectiveResult{
			Applied: true, Resolution: resolution,
			Additional: []regapi.ScopedOperation{{
				Operation: regapi.Operation{Kind: regapi.EntryCreate, Entry: derived}, Scope: regapi.ScopeBaseline,
			}},
			Effects: []regapi.Effect{effect},
		}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	changes := previewDependencyChange()
	preview, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, changes)
	require.NoError(t, err)
	require.Len(t, effects, 1)
	selection = "two"
	transitions := runner.TransitionCount()

	_, err = reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changes)
	requirePreviewApplyConflict(t, err)
	require.Equal(t, transitions, runner.TransitionCount(), "review drift must not reach the runner")
	require.Len(t, effects, 2)
	require.Equal(t, 1, effects[1].rollback, "re-expansion staging must be released")
	require.Zero(t, effects[1].prepare, "digest conflict must precede activation")
	require.Zero(t, effects[1].commit)
	require.Zero(t, effects[1].finalize)
}

func TestApplyPreviewRejectsChangedEffectTargetBeforeActivation(t *testing.T) {
	ctx := context.Background()
	target := "cache/generation-one"
	var effects []*previewEffect
	directive := directiveFunc(func(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		effect := &previewEffect{target: target}
		effects = append(effects, effect)
		return regapi.DirectiveResult{Applied: true, Resolution: previewResolution(op.Entry.ID), Effects: []regapi.Effect{effect}}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	changes := previewDependencyChange()
	preview, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, changes)
	require.NoError(t, err)
	target = "cache/generation-two"
	transitions := runner.TransitionCount()

	_, err = reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changes)
	requirePreviewApplyConflict(t, err)
	require.Equal(t, transitions, runner.TransitionCount())
	require.Len(t, effects, 2)
	require.Equal(t, 1, effects[1].rollback)
	require.Zero(t, effects[1].prepare)
}

func TestPreviewRejectsNilAndUnmeasuredEffects(t *testing.T) {
	ctx := context.Background()
	for _, effect := range []regapi.Effect{nil, (*previewEffect)(nil), &unmeasuredPreviewEffect{}} {
		directive := directiveFunc(func(_ context.Context, _ regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
			return regapi.DirectiveResult{Applied: true, Effects: []regapi.Effect{effect}}, nil
		})
		reg, _ := previewRegistry(t, directive)
		require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
		_, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, previewDependencyChange())
		require.ErrorContains(t, err, "measure registry preview effect")
	}
}

type unmeasuredPreviewEffect struct{}

func (*unmeasuredPreviewEffect) Prepare(context.Context) error  { return nil }
func (*unmeasuredPreviewEffect) Commit(context.Context) error   { return nil }
func (*unmeasuredPreviewEffect) Rollback(context.Context) error { return nil }

func TestApplyPreviewRejectsChangedRequestedRootParameters(t *testing.T) {
	ctx := context.Background()
	var effects []*previewEffect
	directive := directiveFunc(func(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		effect := &previewEffect{}
		effects = append(effects, effect)
		return regapi.DirectiveResult{Applied: true, Resolution: previewResolution(op.Entry.ID), Effects: []regapi.Effect{effect}}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	changes := previewDependencyChange()
	preview, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, changes)
	require.NoError(t, err)
	changed := clonePreviewChanges(changes)
	changed[0].Entry.Data.Data().(map[string]any)["parameter"] = "different-binding"
	transitions := runner.TransitionCount()

	_, err = reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changed)
	requirePreviewApplyConflict(t, err)
	require.Equal(t, transitions, runner.TransitionCount())
	require.Len(t, effects, 2)
	require.Equal(t, 1, effects[1].rollback)
	require.Zero(t, effects[1].prepare)
}

func TestApplyPreviewRejectsStaleBaseAndMalformedDigestBeforeExpansion(t *testing.T) {
	ctx := context.Background()
	calls := 0
	directive := directiveFunc(func(_ context.Context, _ regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		calls++
		return regapi.DirectiveResult{Applied: true}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	changes := previewDependencyChange()
	preview, err := reg.PreviewAt(ctx, reg.Snapshot().Revision, changes)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	_, err = reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: regapi.Entry{
		ID: regapi.NewID("app", "intervening"), Kind: regapi.EntryKind,
	}}})
	require.NoError(t, err)
	transitions := runner.TransitionCount()

	_, err = reg.ApplyPreview(ctx, preview.Revision, preview.Digest, changes)
	requirePreviewApplyConflict(t, err)
	require.Equal(t, 1, calls, "stale base must reject before re-expansion")
	require.Equal(t, transitions, runner.TransitionCount())

	current := reg.Snapshot()
	_, err = reg.ApplyPreview(ctx, current.Revision, "not-a-digest", changes)
	require.ErrorContains(t, err, "measured registry preview is required")
	require.Equal(t, 1, calls, "malformed digest must reject before re-expansion")
	require.Equal(t, transitions, runner.TransitionCount())
}
