package hub

import (
	"context"
	"errors"
	"fmt"

	regapi "github.com/wippyai/runtime/api/registry"
)

type recordedSelectionKey struct{}
type recordedSelection struct {
	previous *regapi.DependencyResolution
	target   *regapi.DependencyResolution
}

func (h *DependencyHandler) ExpandRecordedChanges(ctx context.Context, changes regapi.ChangeSet, snapshot regapi.State, previous, target *regapi.DependencyResolution) (regapi.DirectiveResult, error) {
	if target == nil || !target.Valid() {
		return regapi.DirectiveResult{}, regapi.ErrInvalidDependencyResolution
	}
	ctx = context.WithValue(ctx, recordedSelectionKey{}, &recordedSelection{previous: previous, target: target})
	result, err := h.ExpandChanges(ctx, changes, snapshot)
	if err != nil {
		return result, err
	}
	if result.Resolution != nil && result.Resolution.Digest != target.Digest {
		err = errors.New("recorded expansion changed the stored dependency graph")
		for _, effect := range result.Effects {
			err = errors.Join(err, effect.Rollback(ctx))
		}
		return regapi.DirectiveResult{}, err
	}
	return result, nil
}

func recordedModules(ctx context.Context, deps []DependencyDefinition) ([]ResolvedModule, bool, error) {
	selected, ok := ctx.Value(recordedSelectionKey{}).(*recordedSelection)
	if !ok {
		return nil, false, nil
	}
	modules, err := resolvedModulesFromStored(selected.target)
	if err != nil {
		return nil, true, err
	}
	for _, dep := range deps {
		version, exists := selectedModuleVersion(modules, dep.Component)
		if !exists || !storedVersionSatisfies(version, dep.Version) {
			return nil, true, fmt.Errorf("stored module selection does not satisfy %s@%s", dep.Component, dep.Version)
		}
	}
	return modules, true, nil
}
