// SPDX-License-Identifier: MPL-2.0

package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wippyai/runtime/api/registry"
	regexp "github.com/wippyai/runtime/system/registry/expansion"
)

// expandLocked is shared by preview and apply. Its returned effects have not
// been prepared and belong to the caller, including their staging resources.
// Caller holds applyMu.
func (r *Reg) expandLocked(ctx context.Context, changes registry.ChangeSet, snapshot registry.State) (*regexp.Planner, *regexp.Plan, error) {
	planner := regexp.NewPlanner(r.directivesByKind, r.resolver, r.log.Named("expansion"))
	plan, err := planner.Expand(ctx, changes, snapshot)
	if err != nil {
		return nil, nil, NewExpandChangesError(err)
	}
	plan.Ops, err = planner.SortOps(snapshot, plan.Ops)
	if err != nil {
		planner.RollbackEffects(ctx, plan.Effects)
		return nil, nil, NewSortChangesError(err)
	}
	return planner, plan, nil
}

func (r *Reg) PreviewAt(ctx context.Context, revision uint64, changes registry.ChangeSet) (result *registry.Preview, resultErr error) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	base := r.Snapshot()
	if revision == 0 || revision != base.Revision {
		return nil, NewSnapshotRevisionConflictError(revision, base.Revision)
	}
	durable, err := r.stateAtVersion(ctx, base.Version)
	if err != nil {
		return nil, err
	}
	changes = clonePreviewChanges(changes)
	canonicalizeChangeSetIDs(changes)
	changes = normalizeRegistryMetadata(changes, durable)
	_, plan, err := r.expandLocked(ctx, changes, durable)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Cleanup retains actor/context values after request cancellation. It
		// never calls Prepare, Commit or Finalize on a preview's effects.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var cleanupErr error
		for i := len(plan.Effects) - 1; i >= 0; i-- {
			if plan.Effects[i] == nil || isNilEffect(plan.Effects[i]) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, plan.Effects[i].Rollback(cleanup))
		}
		if cleanupErr != nil {
			result = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("release registry preview staging: %w", cleanupErr))
		}
	}()
	all, history := plan.SplitScopes()
	all, err = r.sortWithIndex(durable, all)
	if err != nil {
		return nil, NewSortChangesError(err)
	}
	resolution := base.Registry.Resolution
	if plan.Resolution != nil {
		resolution = plan.Resolution
	}
	digest, err := previewDigest(all, history, resolution, plan.Effects)
	if err != nil {
		return nil, err
	}
	return &registry.Preview{
		Digest:  digest,
		Version: base.Version, Revision: base.Revision,
		Changes: clonePreviewChanges(all), History: clonePreviewChanges(history),
		Resolution: resolution.Canonical(),
	}, nil
}

func clonePreviewChanges(changes registry.ChangeSet) registry.ChangeSet {
	result := append(registry.ChangeSet(nil), changes...)
	for i := range result {
		result[i].Entry = cloneOverlayEntry(result[i].Entry)
		if result[i].OriginalEntry != nil {
			original := cloneOverlayEntry(*result[i].OriginalEntry)
			result[i].OriginalEntry = &original
		}
	}
	return result
}
