package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/wippyai/runtime/api/registry"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/internal/version"
	legacy "github.com/wippyai/runtime/system/registry/history/postgres"
	"github.com/wippyai/runtime/system/registry/topology"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (h *History) Versions() ([]registry.Version, error) {
	return h.VersionsContext(context.Background())
}

func (h *History) VersionsContext(ctx context.Context) ([]registry.Version, error) {
	root := version.New(0)
	versions := []registry.Version{root}
	byID := map[uint64]registry.Version{0: root}
	after := uint64(0)
	for {
		callCtx, cancel := context.WithTimeout(ctx, h.timeout)
		page, err := h.client.ListVersions(callCtx, &historyv1.ReadRequest{Key: h.key, AfterRevision: after, Limit: 0})
		cancel()
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("empty history metadata response")
		}
		for _, metadata := range page.Versions {
			if metadata == nil || uint64(uint(metadata.Revision)) != metadata.Revision {
				return nil, errors.New("invalid history version metadata")
			}
			if metadata.Revision == 0 {
				if metadata.ParentRevision != 0 {
					return nil, errors.New("root version has a parent")
				}
				continue
			}
			if metadata.Revision <= after {
				return nil, errors.New("history version page is not ordered")
			}
			if _, duplicate := byID[metadata.Revision]; duplicate {
				return nil, errors.New("duplicate history version")
			}
			parent, ok := byID[metadata.ParentRevision]
			if !ok || metadata.ParentRevision >= metadata.Revision {
				return nil, errors.New("history version parent is missing")
			}
			stored := version.FromParent(parent, uint(metadata.Revision))
			versions = append(versions, stored)
			byID[metadata.Revision] = stored
		}
		if !page.HasMore {
			return versions, nil
		}
		if page.NextRevision <= after {
			return nil, errors.New("history version cursor did not advance")
		}
		after = page.NextRevision
	}
}

func (h *History) GetVersion(id uint) (registry.Version, error) {
	return h.GetVersionContext(context.Background(), id)
}

func (h *History) GetVersionContext(ctx context.Context, id uint) (registry.Version, error) {
	versions, err := h.VersionsContext(ctx)
	if err != nil {
		return nil, err
	}
	for _, stored := range versions {
		if stored.ID() == id {
			return stored, nil
		}
	}
	return nil, status.Error(codes.NotFound, "history version not found")
}

func (h *History) Get(target registry.Version) (registry.ChangeSet, error) {
	return h.GetContext(context.Background(), target)
}

func (h *History) GetContext(ctx context.Context, target registry.Version) (registry.ChangeSet, error) {
	if target == nil {
		return nil, errors.New("target version is required")
	}
	current, err := h.readVersion(ctx, uint64(target.ID()), true)
	if err != nil {
		return nil, err
	}
	next, err := decodeVersion(current)
	if err != nil {
		return nil, err
	}
	var previous *registry.PublishedState
	if current.Revision != 0 {
		parent, err := h.readVersion(ctx, current.ParentRevision, true)
		if err != nil {
			return nil, err
		}
		previous, err = decodeVersion(parent)
		if err != nil {
			return nil, err
		}
	}
	oldState := liveEntries(previous)
	if len(current.LegacyChangeset) > 0 {
		return h.decodeLegacyChanges(ctx, current.LegacyChangeset, oldState)
	}
	if current.Revision == 0 {
		return nil, nil
	}
	changes, err := topology.NewStateBuilder(zap.NewNop(), nil).BuildPublishedDelta(oldState, liveEntries(next))
	if err != nil {
		return nil, err
	}
	originals := topology.NewStateMap(oldState)
	for i := range changes {
		if entry, ok := originals[changes[i].Entry.ID]; ok {
			changes[i].OriginalEntry = &entry
		}
	}
	return changes, nil
}

func (h *History) decodeLegacyChanges(ctx context.Context, data []byte, previous registry.State) (registry.ChangeSet, error) {
	h.legacyMu.Lock()
	defer h.legacyMu.Unlock()
	if h.legacyDecoder == nil {
		root, err := h.readVersion(ctx, 0, true)
		if err != nil {
			return nil, err
		}
		baseline, err := decodeVersion(root)
		if err != nil {
			return nil, err
		}
		h.legacyDecoder = legacy.NewLegacyDecoder(liveEntries(baseline))
	}
	changes, err := h.legacyDecoder.Decode(data)
	if err != nil {
		return nil, err
	}
	state := topology.NewStateMap(previous)
	for i := range changes {
		operation := &changes[i]
		if operation.OriginalEntry == nil && (operation.Kind == registry.EntryUpdate || operation.Kind == registry.EntryDelete) {
			if original, exists := state[operation.Entry.ID]; exists {
				operation.OriginalEntry = &original
			}
		}
		switch operation.Kind {
		case registry.EntryCreate, registry.EntryUpdate:
			state[operation.Entry.ID] = operation.Entry
		case registry.EntryDelete:
			delete(state, operation.Entry.ID)
		default:
			return nil, errors.New("invalid legacy history operation")
		}
	}
	return changes, nil
}

func (h *History) ReplayChanges(ctx context.Context, target registry.Version, apply func(registry.ChangeSet) error) error {
	if target == nil || apply == nil {
		return errors.New("target version and callback are required")
	}
	raw, err := h.readVersion(ctx, uint64(target.ID()), true)
	if err != nil {
		return err
	}
	published, err := decodeVersion(raw)
	if err != nil {
		return err
	}
	live := published.Changes[:0]
	for _, operation := range published.Changes {
		if operation.Kind == registry.EntryDelete {
			continue
		}
		operation.Kind = registry.EntryCreate
		live = append(live, operation)
	}
	return apply(live)
}

func (h *History) readVersion(ctx context.Context, revision uint64, exact bool) (*historyv1.Version, error) {
	callCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	result, err := h.client.GetVersion(callCtx, &historyv1.GetRequest{Key: h.key, Revision: revision, Exact: exact}, grpc.MaxCallRecvMsgSize(h.maxMessageBytes))
	if exact && revision == 0 && status.Code(err) == codes.NotFound {
		if _, err := h.readVersion(ctx, 0, false); err != nil {
			return nil, err
		}
		return &historyv1.Version{}, nil
	}
	if err != nil {
		return nil, err
	}
	if result == nil || exact && result.Revision != revision {
		return nil, fmt.Errorf("invalid response for history revision %d", revision)
	}
	return result, nil
}

func liveEntries(published *registry.PublishedState) registry.State {
	if published == nil {
		return nil
	}
	entries := make(registry.State, 0, len(published.Changes))
	for _, operation := range published.Changes {
		if operation.Kind != registry.EntryDelete {
			entries = append(entries, operation.Entry)
		}
	}
	return entries
}

func (h *History) SnapshotAt(ctx context.Context, target registry.Version) (registry.State, error) {
	if target == nil {
		return nil, errors.New("target version is required")
	}
	raw, err := h.readVersion(ctx, uint64(target.ID()), true)
	if err != nil {
		return nil, err
	}
	published, err := decodeVersion(raw)
	if err != nil {
		return nil, err
	}
	return liveEntries(published), nil
}
