package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/attrs"
	"github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	"github.com/wippyai/runtime/api/registry/history/migration"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"google.golang.org/protobuf/proto"
)

func encodeRecordedOperations(t *testing.T, changes registry.ChangeSet) []byte {
	t.Helper()
	records := make([]map[string]any, len(changes))
	for i, operation := range changes {
		entry := map[string]any{"ID": operation.Entry.ID, "Kind": operation.Entry.Kind, "Meta": operation.Entry.Meta, "Registry": operation.Entry.Registry}
		if operation.Entry.Data != nil {
			entry["Data"] = map[string]any{"Data": operation.Entry.Data.Data(), "Format": operation.Entry.Data.Format()}
		}
		records[i] = map[string]any{"Kind": operation.Kind, "Entry": entry}
	}
	var data []byte
	require.NoError(t, codec.NewEncoderBytes(&data, &codec.MsgpackHandle{}).Encode(records))
	return data
}

func TestMaterializeHistoryRetainsBranchesAndRawRecords(t *testing.T) {
	entry := registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind}
	raw := []migration.Record{
		{Changes: encodeRecordedOperations(t, nil)},
		{Revision: 1, Changes: encodeRecordedOperations(t, registry.ChangeSet{{Kind: registry.EntryCreate, Entry: entry}})},
		{Revision: 2, Parent: 1, Changes: encodeRecordedOperations(t, registry.ChangeSet{{Kind: registry.EntryDelete, Entry: entry}})},
		{Revision: 3, Parent: 1, Changes: encodeRecordedOperations(t, nil)},
	}
	var source, output bytes.Buffer
	sum := sha256.Sum256(nil)
	header := migration.Header{Format: migration.RawFormat, BaselineDigest: sum[:], Manifest: migration.Manifest{Count: 4, Maximum: 3, Head: 2}}
	writer, err := migration.NewWriter(&source, header, 65536)
	require.NoError(t, err)
	for i := range raw {
		require.NoError(t, writer.Write(&raw[i]))
	}
	require.NoError(t, writer.Close())
	require.NoError(t, MaterializeHistory(context.Background(), &source, &output, MaterializeHistoryOptions{MaximumRecordBytes: 65536, ScratchDir: t.TempDir(), Hub: &fakeHub{}}))
	reader, err := migration.NewReader(&output, 65536)
	require.NoError(t, err)
	require.Equal(t, migration.MaterializedFormat, reader.Header().Format)
	for i := range raw {
		record, err := reader.Next()
		require.NoError(t, err)
		snapshot := new(historyv1.Version)
		require.NoError(t, proto.Unmarshal(record.Snapshot, snapshot))
		require.Equal(t, raw[i].Revision, snapshot.Revision)
		require.Equal(t, raw[i].Parent, snapshot.ParentRevision)
		if i == 0 {
			require.Empty(t, snapshot.LegacyChangeset)
		} else {
			require.Equal(t, raw[i].Changes, snapshot.LegacyChangeset)
		}
		record.Snapshot = nil
		require.Equal(t, raw[i], *record)
		if i == 0 {
			require.Empty(t, snapshot.Entries)
			continue
		}
		require.Len(t, snapshot.Entries, 1)
		require.Equal(t, i == 2, snapshot.Entries[0].Deleted)
		if i != 2 {
			decoded, err := entryencoding.DecodeEntry(snapshot.Entries[0].Value)
			require.NoError(t, err)
			require.Equal(t, entry, decoded)
		}
	}
	_, err = reader.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestMaterializeHistoryRejectsMissingGraphAndInvalidProof(t *testing.T) {
	for _, test := range []string{"baseline digest", "root graph", "non-root graph", "source format", "cancelled"} {
		t.Run(test, func(t *testing.T) {
			root := registry.Entry{ID: registry.NewID("host", "module"), Kind: registry.NamespaceDependency}
			value, err := entryencoding.EncodeEntry(root)
			require.NoError(t, err)
			baseline := &historyv1.Version{}
			if test == "root graph" {
				baseline.Entries = []*historyv1.Mutation{{EntryId: root.ID.String(), Value: value}}
			}
			data, err := proto.MarshalOptions{Deterministic: true}.Marshal(baseline)
			require.NoError(t, err)
			sum := sha256.Sum256(data)
			header := migration.Header{Format: migration.RawFormat, Baseline: data, BaselineDigest: sum[:], Manifest: migration.Manifest{Count: 1}}
			if test == "baseline digest" {
				header.BaselineDigest = nil
			}
			if test == "source format" {
				header.Format = migration.MaterializedFormat
			}
			if test == "non-root graph" {
				header.Manifest = migration.Manifest{Count: 2, Maximum: 1, Head: 1}
			}
			var source, output bytes.Buffer
			writer, err := migration.NewWriter(&source, header, 65536)
			require.NoError(t, err)
			require.NoError(t, writer.Write(&migration.Record{}))
			if test == "non-root graph" {
				require.NoError(t, writer.Write(&migration.Record{Revision: 1, Changes: encodeRecordedOperations(t, registry.ChangeSet{{Kind: registry.EntryCreate, Entry: root}})}))
			}
			require.NoError(t, writer.Close())
			ctx := context.Background()
			if test == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			require.Error(t, MaterializeHistory(ctx, &source, &output, MaterializeHistoryOptions{MaximumRecordBytes: 65536, ScratchDir: t.TempDir(), Hub: &fakeHub{}}))
		})
	}
}

func TestMaterializeHistoryRejectsInvalidTopology(t *testing.T) {
	first := registry.Entry{ID: registry.NewID("test", "first"), Kind: registry.EntryKind, Meta: attrs.NewBagFrom(map[string]any{"depends_on": "test:second"})}
	second := registry.Entry{ID: registry.NewID("test", "second"), Kind: registry.EntryKind, Meta: attrs.NewBagFrom(map[string]any{"depends_on": "test:first"})}
	baseline := &historyv1.Version{}
	for _, entry := range []registry.Entry{first, second} {
		value, err := entryencoding.EncodeEntry(entry)
		require.NoError(t, err)
		baseline.Entries = append(baseline.Entries, &historyv1.Mutation{EntryId: entry.ID.String(), Value: value})
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(baseline)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	var source, output bytes.Buffer
	writer, err := migration.NewWriter(&source, migration.Header{Format: migration.RawFormat, Baseline: data, BaselineDigest: digest[:], Manifest: migration.Manifest{Count: 1}}, 65536)
	require.NoError(t, err)
	require.NoError(t, writer.Write(&migration.Record{}))
	require.NoError(t, writer.Close())
	require.Error(t, MaterializeHistory(t.Context(), &source, &output, MaterializeHistoryOptions{MaximumRecordBytes: 65536, ScratchDir: t.TempDir(), Hub: &fakeHub{}}))
}
