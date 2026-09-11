package encoding

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/attrs"
	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
)

func TestEntryRoundTrip(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: "registry.entry", Meta: attrs.Bag{"enabled": true}, Registry: registry.EntryMetadata{Owner: "test/source", Root: true}, Data: payload.NewPayload([]byte{0, 128, 255}, payload.Bytes)}
	data, err := EncodeEntry(entry)
	require.NoError(t, err)
	restored, err := DecodeEntry(data)
	require.NoError(t, err)
	require.Equal(t, entry, restored)
	again, err := EncodeEntry(restored)
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestEntryRejectsUnknownEncodingAndTrailingData(t *testing.T) {
	_, err := DecodeEntry([]byte{2, 0})
	require.Error(t, err)
	data, err := EncodeEntry(registry.Entry{ID: registry.NewID("test", "entry"), Kind: "registry.entry"})
	require.NoError(t, err)
	_, err = DecodeEntry(append(data, 0))
	require.Error(t, err)
	_, err = DecodeEntry(bytes.Repeat([]byte{0}, MaxEntryBytes+1))
	require.Error(t, err)
}

func TestEntryRejectsInvalidIdentity(t *testing.T) {
	_, err := EncodeEntry(registry.Entry{})
	require.Error(t, err)
}

func BenchmarkEntryRoundTrip(b *testing.B) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: "registry.entry", Data: payload.New(map[string]any{"data": "value"})}
	b.ReportAllocs()
	for b.Loop() {
		data, err := EncodeEntry(entry)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := DecodeEntry(data); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEntryRetainsDependencyMetadataStrings(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Meta: attrs.Bag{"depends_on": []string{"test:other"}}}
	data, err := EncodeEntry(entry)
	require.NoError(t, err)
	decoded, err := DecodeEntry(data)
	require.NoError(t, err)
	require.Equal(t, []string{"test:other"}, decoded.Meta.GetSlice("depends_on"))
}
