package hub

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
)

func TestValidateDependencyResolutionRejectsMissingAndChangedInputs(t *testing.T) {
	ctx := newTestContext()
	transcoder := payload.GetTranscoder(ctx)
	root := regapi.Entry{ID: regapi.NewID("app", "root"), Kind: regapi.NamespaceDependency, Data: payload.New(map[string]any{"component": "acme/worker", "version": "^1.0.0"})}
	desired := []desiredDependency{{entry: root, definition: DependencyDefinition{Component: "acme/worker", Version: "^1.0.0"}}}
	resolution := dependencyResolution(desired, nil, []ResolvedModule{{Org: "acme", Name: "worker", Version: "1.2.3", Digest: "sha256:fixture"}})
	require.NoError(t, ValidateDependencyResolution(ctx, regapi.State{root}, resolution, transcoder))
	require.Error(t, ValidateDependencyResolution(ctx, regapi.State{root}, nil, transcoder))
	changed := root
	changed.Data = payload.New(map[string]any{"component": "acme/other", "version": "^1.0.0"})
	require.Error(t, ValidateDependencyResolution(ctx, regapi.State{changed}, resolution, transcoder))
	changed.Data = payload.New(map[string]any{"component": "acme/worker", "version": "^2.0.0"})
	require.Error(t, ValidateDependencyResolution(ctx, regapi.State{changed}, resolution, transcoder))
	require.Error(t, ValidateDependencyResolution(ctx, nil, resolution, transcoder))
	modified := *resolution
	modified.InputDigest = "wrong"
	require.Error(t, ValidateDependencyResolution(ctx, regapi.State{root}, modified.Canonical(), transcoder))
}

func TestValidateDependencyResolutionPermitsFoldedReferencesAndOwnedTransitiveEntries(t *testing.T) {
	ctx := newTestContext()
	transcoder := payload.GetTranscoder(ctx)
	root := regapi.Entry{ID: regapi.NewID("app", "root"), Kind: regapi.NamespaceDependency, Registry: regapi.EntryMetadata{Owner: "app/root", Root: true}, Data: payload.New(map[string]any{"component": "acme/worker", "version": "^1.0.0"})}
	reference := regapi.Entry{ID: regapi.NewID("app", "reference"), Kind: regapi.NamespaceDependency, Registry: regapi.EntryMetadata{Owner: "app/root", Root: true}, Data: payload.New(map[string]any{"component": "acme/worker"})}
	transitive := regapi.Entry{ID: regapi.NewID("worker", "dependency"), Kind: regapi.NamespaceDependency, Registry: regapi.EntryMetadata{Owner: "acme/worker"}, Data: payload.New(map[string]any{"component": "acme/transitive", "version": "*"})}
	roots := []desiredDependency{{entry: root, definition: DependencyDefinition{Component: "acme/worker", Version: "^1.0.0"}}}
	references := []desiredDependency{{entry: reference, definition: DependencyDefinition{Component: "acme/worker"}}}
	resolution := dependencyResolution(roots, references, []ResolvedModule{{Org: "acme", Name: "worker", Version: "1.2.3", Digest: "sha256:fixture"}})
	require.NoError(t, ValidateDependencyResolution(ctx, regapi.State{root, reference, transitive}, resolution, transcoder))
	require.NoError(t, ValidateDependencyResolution(ctx, regapi.State{transitive}, nil, transcoder))
	require.Error(t, ValidateDependencyResolution(ctx, regapi.State{root, reference}, dependencyResolution(roots, nil, []ResolvedModule{{Org: "acme", Name: "worker", Version: "1.2.3", Digest: "sha256:fixture"}}), transcoder))
}
