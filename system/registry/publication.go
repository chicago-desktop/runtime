package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/wippyai/runtime/api/registry"
	regexp "github.com/wippyai/runtime/system/registry/expansion"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
)

func (r *Reg) submitPublished(ctx context.Context, history registry.PublishedHistory, changes registry.ChangeSet) (registry.Version, error) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	if len(changes) == 0 {
		return r.Current()
	}
	r.mu.RLock()
	snapshot := append(registry.State(nil), r.state...)
	resolution := r.currentResolution
	r.mu.RUnlock()
	operations, graph, err := r.planSubmission(ctx, changes, snapshot, resolution)
	if err != nil {
		return nil, err
	}
	receipt, err := history.SubmitChanges(ctx, operations, graph)
	if err != nil {
		return nil, err
	}
	published, err := history.AwaitPublished(ctx, receipt)
	if err != nil {
		return nil, err
	}
	err = r.applyPublication(ctx, history, published, r.baseline, false)
	return published.Version, err
}

func (r *Reg) planSubmission(ctx context.Context, changes registry.ChangeSet, snapshot registry.State, resolution *registry.DependencyResolution) (registry.ChangeSet, *registry.DependencyResolution, error) {
	changes = append(registry.ChangeSet(nil), changes...)
	canonicalizeChangeSetIDs(changes)
	changes = normalizeRegistryMetadata(changes, snapshot)
	if len(r.directivesByKind) > 0 {
		planner := regexp.NewPlanner(r.directivesByKind, r.resolver, r.log.Named("expansion"))
		plan, err := planner.Expand(ctx, changes, snapshot)
		if err != nil {
			return nil, nil, err
		}
		defer planner.RollbackEffects(ctx, plan.Effects)
		plan.Ops, err = planner.SortOps(snapshot, plan.Ops)
		if err != nil {
			return nil, nil, err
		}
		all, _ := plan.SplitScopes()
		if err := r.validateDurableTransitionAgainstOverlays(all); err != nil {
			return nil, nil, err
		}
		if err := r.validatePublicationOperations(snapshot, all); err != nil {
			return nil, nil, err
		}
		changes = all
		if plan.Resolution != nil {
			resolution = plan.Resolution.Canonical()
		}
	} else {
		sorted, err := r.sortWithIndex(snapshot, changes)
		if err != nil {
			return nil, nil, err
		}
		changes = sorted
		if err := r.validateDurableTransitionAgainstOverlays(changes); err != nil {
			return nil, nil, err
		}
		if err := r.validatePublicationOperations(snapshot, changes); err != nil {
			return nil, nil, err
		}
	}
	return changes, resolution, nil
}

func (r *Reg) validatePublicationOperations(snapshot registry.State, changes registry.ChangeSet) error {
	handler, ok := r.builder.(registry.OperationHandler)
	if !ok {
		handler = topology.NewStateBuilder(r.log, r.resolver)
	}
	state := make(registry.StateMap, len(snapshot)+len(changes))
	for _, entry := range snapshot {
		state[entry.ID] = entry
	}
	for i, operation := range changes {
		if err := handler.ValidateOperation(state, operation); err != nil {
			return err
		}
		applyStateOperations(state, changes[i:i+1])
	}
	return nil
}

func (r *Reg) restorePublished(ctx context.Context, history registry.PublishedHistory, target registry.Version) error {
	if target == nil {
		return errors.New("restore target is required")
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	receipt, err := history.RestoreChanges(ctx, uint64(target.ID()))
	if err != nil {
		return err
	}
	published, err := history.AwaitPublished(ctx, receipt)
	if err != nil {
		return err
	}
	return r.applyPublication(ctx, history, published, r.baseline, false)
}

func (r *Reg) loadPublished(ctx context.Context, history registry.PublishedHistory, baseline registry.State, target registry.Version) error {
	if target == nil {
		return errors.New("load target is required")
	}
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	baseline = append(registry.State(nil), baseline...)
	for i := range baseline {
		baseline[i].ID = canonicalEntryID(baseline[i].ID)
	}
	if err := topology.ValidateUniqueEntryIDs("baseline", baseline); err != nil {
		return err
	}
	published, err := history.ReadPublished(ctx, uint64(target.ID()))
	if err != nil {
		return err
	}
	if published.Version.ID() == registry.RootVersion && len(published.Changes) == 0 && published.Resolution == nil && len(baseline) > 0 {
		initial := make(registry.ChangeSet, len(baseline))
		for i, entry := range baseline {
			initial[i] = registry.Operation{Kind: registry.EntryUpdate, Entry: entry}
		}
		operations, graph, planErr := r.planSubmission(ctx, initial, baseline, nil)
		if planErr != nil {
			return planErr
		}
		receipt, submitErr := history.SubmitChanges(ctx, operations, graph)
		if submitErr != nil {
			return submitErr
		}
		published, err = history.AwaitPublished(ctx, receipt)
		if err != nil {
			return err
		}
	}
	return r.applyPublication(ctx, history, published, baseline, true)
}

func (r *Reg) FollowPublications(ctx context.Context) error {
	history, ok := r.history.(registry.PublishedHistory)
	if !ok {
		return registry.ErrHistoryOperationUnsupported
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.publicationReady:
	}
	current, err := r.Current()
	if err != nil {
		return err
	}
	return history.FollowPublished(ctx, uint64(current.ID()), func(published *registry.PublishedState) error {
		r.applyMu.Lock()
		defer r.applyMu.Unlock()
		return r.applyPublication(ctx, history, published, r.baseline, false)
	})
}

func (r *Reg) applyPublication(ctx context.Context, history registry.PublishedHistory, published *registry.PublishedState, baseline registry.State, reset bool) (resultErr error) {
	if published == nil || published.Version == nil {
		return errors.New("published state is required")
	}
	r.mu.RLock()
	snapshot := append(registry.State(nil), r.state...)
	current := r.currentVersion
	r.mu.RUnlock()
	if reset && current != nil && current.ID() > published.Version.ID() {
		return fmt.Errorf("cannot load published revision %d before current revision %d", published.Version.ID(), current.ID())
	}
	if !reset && current != nil && current.ID() > published.Version.ID() {
		return nil
	}
	defer func() {
		if published.Version.ID() == registry.RootVersion {
			return
		}
		reportErr := history.ReportApplied(context.WithoutCancel(ctx), published, resultErr)
		if reportErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("report applied revision %d: %w", published.Version.ID(), reportErr))
		}
	}()
	if !reset && current != nil && current.ID() == published.Version.ID() {
		return nil
	}
	state := make(registry.StateMap, len(published.Changes))
	applyStateOperations(state, published.Changes)
	var planner *regexp.Planner
	var effects []registry.Effect
	var deleted map[registry.ID]struct{}
	if len(r.directivesByKind) > 0 {
		planner = regexp.NewPlanner(r.directivesByKind, r.resolver, r.log.Named("expansion"))
		for _, operation := range published.Changes {
			if operation.Kind == registry.EntryDelete {
				if deleted == nil {
					deleted = make(map[registry.ID]struct{})
				}
				deleted[operation.Entry.ID] = struct{}{}
			}
		}
	}
	if published.Resolution != nil {
		if planner == nil {
			return errors.New("published dependency resolution requires a configured reconciler")
		}
		reconciled := false
		for _, directive := range r.directivesByKind[registry.NamespaceDependency] {
			reconciliation, ok, err := reconcileStoredResolution(ctx, directive, snapshot, topology.StateMapToSlice(state), published.Resolution)
			if !ok {
				continue
			}
			if err != nil {
				return err
			}
			if reconciliation.Resolution != nil && reconciliation.Resolution.Canonical().Digest != published.Resolution.Digest {
				planner.RollbackEffects(ctx, reconciliation.Effects)
				return errors.New("dependency graph rebase requires a new published change")
			}
			reconciled = reconciled || reconciliation.Applied
			effects = append(effects, reconciliation.Effects...)
			for _, operation := range reconciliation.Additional {
				if err := validatePublishedEntry(state, deleted, operation.Operation); err != nil {
					planner.RollbackEffects(ctx, effects)
					return err
				}
			}
		}
		if !reconciled {
			planner.RollbackEffects(ctx, effects)
			return errors.New("published dependency resolution has no configured reconciler")
		}
	} else if planner != nil {
		for _, entry := range topology.StateMapToSlice(state) {
			if len(r.directivesByKind[entry.Kind]) == 0 {
				continue
			}
			plan, err := planner.Expand(ctx, registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: entry}}, topology.StateMapToSlice(state))
			if err != nil {
				planner.RollbackEffects(ctx, effects)
				return err
			}
			if plan.Resolution != nil {
				planner.RollbackEffects(ctx, plan.Effects)
				planner.RollbackEffects(ctx, effects)
				return errors.New("dependency resolution requires publication before local application")
			}
			effects = append(effects, plan.Effects...)
			for _, operation := range plan.Ops {
				if err := validatePublishedEntry(state, deleted, operation.Operation); err != nil {
					planner.RollbackEffects(ctx, effects)
					return err
				}
			}
		}
	}
	committed := false
	defer func() {
		if planner != nil && !committed {
			planner.RollbackEffects(ctx, effects)
		}
	}()
	if !reset {
		if err := r.composeOverlays(state); err != nil {
			return err
		}
	}
	buildDelta := r.builder.BuildDelta
	if builder, ok := r.builder.(interface {
		BuildPublishedDelta(registry.State, registry.State) (registry.ChangeSet, error)
	}); ok {
		buildDelta = builder.BuildPublishedDelta
	}
	operations, err := buildDelta(snapshot, topology.StateMapToSlice(state))
	if err != nil {
		return err
	}
	operations, err = r.sortWithIndex(snapshot, operations)
	if err != nil {
		return err
	}
	if planner != nil {
		if _, err := planner.PrepareEffects(ctx, effects); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	newState, err := r.runner.Transition(ctx, r.state, operations)
	if err != nil {
		if newState != nil && ctx.Err() == nil {
			err = errors.Join(err, r.rollback(ctx, newState, r.state))
		}
		return err
	}
	if planner != nil {
		if err := planner.CommitEffects(ctx, effects); err != nil {
			return errors.Join(err, r.rollback(ctx, newState, r.state))
		}
	}
	r.state = newState
	r.currentVersion = published.Version
	r.currentResolution = published.Resolution
	if reset {
		r.baseline = baseline
		r.overlays = make(map[string]registry.State)
		r.overlayOwners = make(map[registry.ID]string)
		r.overlayGeneration = make(map[string]uint64)
		if r.stateLoaded || r.overlayEpoch > 0 {
			r.overlayEpoch++
		}
		r.overlayFloor = r.overlayEpoch
		if !r.stateLoaded {
			close(r.publicationReady)
		}
		r.stateLoaded = true
	}
	r.rebuildIndex()
	r.rebuildDepIndex()
	r.publishSnapshot()
	committed = true
	if planner != nil {
		if err := planner.FinalizeEffects(ctx, effects); err != nil {
			r.log.Warn("failed to finalize published effects", zap.Error(err))
		}
	}
	return nil
}

func validatePublishedEntry(state registry.StateMap, deleted map[registry.ID]struct{}, operation registry.Operation) error {
	if operation.Kind == registry.EntryDelete {
		return nil
	}
	if _, exists := state[operation.Entry.ID]; exists {
		return nil
	}
	if _, exists := deleted[operation.Entry.ID]; exists {
		return nil
	}
	return fmt.Errorf("published snapshot is missing derived entry %s", operation.Entry.ID.String())
}
