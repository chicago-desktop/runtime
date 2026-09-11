package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBundleRetainsRawFieldsAndBranches(t *testing.T) {
	header := Header{Format: MaterializedFormat, Manifest: Manifest{Digest: []byte("source"), Head: 2, Maximum: 3, Count: 4}, BaselineDigest: []byte("baseline"), Baseline: []byte("root")}
	graph := "graph"
	records := []*Record{{}, {Revision: 1, Changes: []byte{0, 1, 2}, Resolution: []byte("graph-bytes"), ResolutionDigest: &graph, Snapshot: []byte("one")}, {Revision: 2, Parent: 1, Snapshot: []byte("two")}, {Revision: 3, Parent: 1, Snapshot: []byte("branch")}}
	var data bytes.Buffer
	writer, err := NewWriter(&data, header, 4096)
	require.NoError(t, err)
	for _, record := range records {
		require.NoError(t, writer.Write(record))
	}
	require.NoError(t, writer.Close())
	reader, err := NewReader(&data, 4096)
	require.NoError(t, err)
	require.Equal(t, header, reader.Header())
	for _, expected := range records {
		actual, err := reader.Next()
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	}
	_, err = reader.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestBundleRejectsInvalidInput(t *testing.T) {
	header, err := json.Marshal(Header{Format: RawFormat, Manifest: Manifest{Count: 1}})
	require.NoError(t, err)
	for _, tail := range []string{"", "{\"unknown\":true}\n", "{} {}\n", "{}\n{}\n", "{\"revision\":1}\n"} {
		t.Run(tail, func(t *testing.T) {
			reader, err := NewReader(strings.NewReader(string(header)+"\n"+tail), 4096)
			require.NoError(t, err)
			for {
				_, err = reader.Next()
				if err != nil {
					break
				}
			}
			require.Error(t, err)
			require.NotErrorIs(t, err, io.EOF)
		})
	}
	_, err = NewReader(strings.NewReader(string(header)+"\n"), len(header))
	require.Error(t, err)
	_, err = NewReader(strings.NewReader("{\"format\":\"unknown\"}\n"), 4096)
	require.Error(t, err)
}

func TestBundleRejectsMissingParentAndIncompleteOutput(t *testing.T) {
	var data bytes.Buffer
	writer, err := NewWriter(&data, Header{Format: RawFormat, Manifest: Manifest{Count: 3, Maximum: 3, Head: 3}}, 4096)
	require.NoError(t, err)
	require.NoError(t, writer.Write(&Record{}))
	require.Error(t, writer.Write(&Record{Revision: 3, Parent: 2}))
	require.Error(t, writer.Close())
}

func TestBundleBoundsRecordBeforeWriting(t *testing.T) {
	var data bytes.Buffer
	writer, err := NewWriter(&data, Header{Format: RawFormat, Manifest: Manifest{Count: 1}}, 4096)
	require.NoError(t, err)
	before := data.Len()
	require.Error(t, writer.Write(&Record{Changes: make([]byte, 4096)}))
	require.Equal(t, before, data.Len())
}

func BenchmarkBundleRead(b *testing.B) {
	var data bytes.Buffer
	writer, err := NewWriter(&data, Header{Format: RawFormat, Manifest: Manifest{Count: 1}}, 4096)
	if err != nil {
		b.Fatal(err)
	}
	if err := writer.Write(&Record{}); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	encoded := data.Bytes()
	b.ReportAllocs()
	for b.Loop() {
		reader, err := NewReader(bytes.NewReader(encoded), 4096)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := reader.Next(); err != nil {
			b.Fatal(err)
		}
		if _, err := reader.Next(); !errors.Is(err, io.EOF) {
			b.Fatal(err)
		}
	}
}
