package postgres

import (
	"testing"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/registry"
)

func TestLegacyDecoderPreservesReleasedMetadata(t *testing.T) {
	id := registry.NewID("test", "entry")
	var data []byte
	records := []releasedMigrationOperation{{Kind: registry.EntryCreate, Entry: releasedMigrationEntry{ID: id, Kind: registry.EntryKind, DependencyRoot: true}, Current: &releasedMigrationRecord{Module: "test/module", Root: true}}}
	require.NoError(t, codec.NewEncoderBytes(&data, newMsgpackHandle()).Encode(records))
	changes, err := NewLegacyDecoder(nil).Decode(data)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Equal(t, registry.EntryMetadata{Owner: "test/module", Root: true}, changes[0].Entry.Registry)
}
