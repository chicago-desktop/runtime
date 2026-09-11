package postgres

import (
	"github.com/wippyai/runtime/api/registry"
	metadatastore "github.com/wippyai/runtime/system/registry/migration/storage"
)

type LegacyDecoder struct {
	baseline map[registry.ID]registry.EntryMetadata
	history  History
}

func NewLegacyDecoder(baseline registry.State) *LegacyDecoder {
	metadata := make(map[registry.ID]registry.EntryMetadata, len(baseline))
	for _, entry := range baseline {
		metadata[entry.ID.Canonical()] = entry.Registry
	}
	return &LegacyDecoder{history: History{handle: newMsgpackHandle()}, baseline: metadata}
}

func (d *LegacyDecoder) Decode(data []byte) (registry.ChangeSet, error) {
	rewritten, _, err := metadatastore.RewriteChangeSet(data, d.history.handle, d.baseline)
	if err != nil {
		return nil, err
	}
	changes, err := d.history.decodeChangeSet(rewritten)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		changes[i].Entry.ID = changes[i].Entry.ID.Canonical()
		if changes[i].OriginalEntry != nil {
			changes[i].OriginalEntry.ID = changes[i].OriginalEntry.ID.Canonical()
		}
	}
	return changes, nil
}
