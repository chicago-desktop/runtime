// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierror "github.com/wippyai/runtime/api/error"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	historymem "github.com/wippyai/runtime/system/registry/history/memory"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

type previewEffect struct {
	rollbackErr                         error
	rollbackContextErr                  error
	target                              string
	prepare, commit, rollback, finalize int
}

func (e *previewEffect) PreviewDigest() (string, error) {
	sum := sha256.Sum256([]byte(e.target))
	return hex.EncodeToString(sum[:]), nil
}

func (e *previewEffect) Prepare(context.Context) error { e.prepare++; return nil }
func (e *previewEffect) Commit(context.Context) error  { e.commit++; return nil }
func (e *previewEffect) Rollback(ctx context.Context) error {
	e.rollback++
	e.rollbackContextErr = ctx.Err()
	return e.rollbackErr
}
func (e *previewEffect) Finalize(context.Context) error { e.finalize++; return nil }

func previewResolution(id regapi.ID) *regapi.DependencyResolution {
	return (&regapi.DependencyResolution{
		InputDigest: "sha256:preview-input",
		Roots: []regapi.DependencyRoot{{
			ID: id.String(), Component: "acme/preview", Version: "v1.0.0",
		}},
		Modules: []regapi.ResolvedModule{{
			Name: "acme/preview", Version: "v1.0.0", Digest: "sha256:preview-artifact",
		}},
	}).Canonical()
}

func previewRegistry(t *testing.T, directive regapi.Directive) (*Reg, *TestRunner) {
	t.Helper()
	resolver := topology.NewResolver()
	builder := topology.NewStateBuilder(zap.NewNop(), resolver)
	runner := NewTestRunner()
	return NewRegistry(historymem.New(), runner, builder, resolver, zap.NewNop(),
		WithKindDirective("preview.dependency", directive)), runner
}

func previewDependencyChange() regapi.ChangeSet {
	return regapi.ChangeSet{{
		Kind: regapi.EntryCreate,
		Entry: regapi.Entry{
			ID: regapi.NewID("app", "preview"), Kind: "preview.dependency",
			Data: payload.New(map[string]any{"component": "acme/preview"}),
		},
	}}
}

func findPreviewOperation(t *testing.T, changes regapi.ChangeSet, id regapi.ID) *regapi.Operation {
	t.Helper()
	for i := range changes {
		if changes[i].Entry.ID == id {
			return &changes[i]
		}
	}
	t.Fatalf("operation for %s not found", id)
	return nil
}

func TestPreviewAtMatchesApplyAndReleasesUnpreparedEffects(t *testing.T) {
	ctx := context.Background()
	var effects []*previewEffect
	derived := regapi.Entry{ID: regapi.NewID("acme.preview", "service"), Kind: regapi.EntryKind,
		Data: payload.New(map[string]any{"derived": true})}
	input := previewDependencyChange()

	directive := directiveFunc(func(_ context.Context, op regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		effect := &previewEffect{}
		effects = append(effects, effect)
		return regapi.DirectiveResult{
			Applied:    true,
			Resolution: previewResolution(op.Entry.ID),
			Additional: []regapi.ScopedOperation{{
				Operation: regapi.Operation{Kind: regapi.EntryCreate, Entry: derived},
				Scope:     regapi.ScopeBaseline,
			}},
			Effects: []regapi.Effect{effect},
		}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	base := reg.Snapshot()
	head, err := reg.History().Head()
	require.NoError(t, err)
	transitions := runner.TransitionCount()

	preview, err := reg.PreviewAt(ctx, base.Revision, input)
	require.NoError(t, err)
	require.Len(t, effects, 1)
	require.Equal(t, base.Version.ID(), preview.Version.ID())
	require.Equal(t, base.Revision, preview.Revision)
	require.Equal(t, previewResolution(input[0].Entry.ID), preview.Resolution)
	require.Equal(t, transitions, runner.TransitionCount(), "preview must not transition the runner")
	require.Equal(t, base, reg.Snapshot(), "preview must not change effective state")
	afterHead, err := reg.History().Head()
	require.NoError(t, err)
	require.Equal(t, head.ID(), afterHead.ID(), "preview must not write history")
	require.Equal(t, 0, effects[0].prepare)
	require.Equal(t, 0, effects[0].commit)
	require.Equal(t, 0, effects[0].finalize)
	require.Equal(t, 1, effects[0].rollback, "preview must release planned effects")

	expectedChanges := clonePreviewChanges(preview.Changes)
	expectedHistory := clonePreviewChanges(preview.History)
	expectedResolution := preview.Resolution.Canonical()
	derivedPreview := findPreviewOperation(t, preview.Changes, derived.ID)
	require.Equal(t, regapi.EntryCreate, derivedPreview.Kind)
	for _, op := range preview.History {
		require.NotEqual(t, derived.ID, op.Entry.ID, "baseline-derived operation must stay out of history")
	}
	require.Len(t, preview.History, 1)
	require.Equal(t, input[0].Entry.ID, preview.History[0].Entry.ID)

	// Preview is an output boundary: a caller cannot alter a later guarded
	// transition by retaining and modifying the plan it was shown.
	previewDependency := findPreviewOperation(t, preview.Changes, input[0].Entry.ID)
	previewDependency.Entry.Data.Data().(map[string]any)["tampered"] = true
	preview.Resolution.Modules[0].Version = "tampered"

	applied, err := reg.ApplyAt(ctx, preview.Revision, input)
	require.NoError(t, err)
	require.Equal(t, uint(1), applied.ID())
	require.Len(t, effects, 2)
	require.Equal(t, 1, effects[1].prepare)
	require.Equal(t, 1, effects[1].commit)
	require.Equal(t, 1, effects[1].finalize)
	require.Zero(t, effects[1].rollback)
	require.Equal(t, expectedChanges, clonePreviewChanges(runner.LastTransition()), "preview must contain the same derived operations Apply executes")

	stored, err := reg.History().Get(applied)
	require.NoError(t, err)
	require.Equal(t, expectedHistory, clonePreviewChanges(stored), "preview history must retain only history-scoped operations")
	require.Equal(t, expectedResolution, reg.Snapshot().Registry.Resolution)
	actualInput := findPreviewOperation(t, runner.LastTransition(), input[0].Entry.ID)
	require.NotContains(t, actualInput.Entry.Data.Data().(map[string]any), "tampered")
}

func TestPreviewAtRejectsStaleRevisionBeforeDirectiveExpansion(t *testing.T) {
	ctx := context.Background()
	calls := 0
	directive := directiveFunc(func(_ context.Context, _ regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		calls++
		return regapi.DirectiveResult{Applied: true}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	stale := reg.Snapshot()
	_, err := reg.Apply(ctx, regapi.ChangeSet{{Kind: regapi.EntryCreate, Entry: regapi.Entry{
		ID: regapi.NewID("app", "intervening"), Kind: regapi.EntryKind,
	}}})
	require.NoError(t, err)
	transitions := runner.TransitionCount()

	_, err = reg.PreviewAt(ctx, stale.Revision, previewDependencyChange())
	var typed apierror.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, apierror.Conflict, typed.Kind())
	require.Zero(t, calls, "a stale preview must not invoke directives")
	require.Equal(t, transitions, runner.TransitionCount())
}

func TestPreviewAtFailsClosedWhenEffectCleanupFails(t *testing.T) {
	ctx := context.Background()
	effect := &previewEffect{rollbackErr: errors.New("staging cleanup failed")}
	directive := directiveFunc(func(_ context.Context, _ regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		return regapi.DirectiveResult{Applied: true, Effects: []regapi.Effect{effect}}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(ctx, nil, version.FromParent(nil, regapi.RootVersion)))
	base := reg.Snapshot()
	transitions := runner.TransitionCount()

	preview, err := reg.PreviewAt(ctx, base.Revision, previewDependencyChange())
	require.Nil(t, preview)
	require.ErrorContains(t, err, "release registry preview staging")
	require.ErrorContains(t, err, "staging cleanup failed")
	require.Equal(t, 1, effect.rollback)
	require.Zero(t, effect.prepare)
	require.Zero(t, effect.commit)
	require.Zero(t, effect.finalize)
	require.Equal(t, base, reg.Snapshot())
	require.Equal(t, transitions, runner.TransitionCount())
}

func TestPreviewAtReleasesEffectsAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	effect := &previewEffect{}
	directive := directiveFunc(func(_ context.Context, _ regapi.Operation, _ regapi.State) (regapi.DirectiveResult, error) {
		cancel()
		return regapi.DirectiveResult{Applied: true, Effects: []regapi.Effect{effect}}, nil
	})
	reg, runner := previewRegistry(t, directive)
	require.NoError(t, reg.LoadState(context.Background(), nil, version.FromParent(nil, regapi.RootVersion)))
	base := reg.Snapshot()
	transitions := runner.TransitionCount()

	preview, err := reg.PreviewAt(ctx, base.Revision, previewDependencyChange())
	require.NoError(t, err)
	require.NotNil(t, preview)
	require.Equal(t, 1, effect.rollback)
	require.NoError(t, effect.rollbackContextErr, "cleanup must outlive request cancellation")
	require.Zero(t, effect.prepare)
	require.Zero(t, effect.commit)
	require.Zero(t, effect.finalize)
	require.Equal(t, base, reg.Snapshot())
	require.Equal(t, transitions, runner.TransitionCount())
}
