package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wippyai/runtime/api/payload"

	"github.com/stretchr/testify/require"
	regapi "github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestHistoryBaselineExportRetainsOwnershipAndCanonicalOrder(t *testing.T) {
	entries := []regapi.Entry{{ID: regapi.NewID("test", "z"), Kind: regapi.EntryKind, Registry: regapi.EntryMetadata{Owner: "test/module", Root: true}}, {ID: regapi.NewID("test", "a"), Kind: regapi.EntryKind}}
	data, err := encodeHistoryBaseline(entries)
	require.NoError(t, err)
	var baseline historyv1.Version
	require.NoError(t, protojson.Unmarshal(data, &baseline))
	require.Zero(t, baseline.Revision)
	require.Len(t, baseline.Entries, 2)
	require.Equal(t, "test:a", baseline.Entries[0].EntryId)
	decoded, err := entryencoding.DecodeEntry(baseline.Entries[1].Value)
	require.NoError(t, err)
	require.Equal(t, entries[0], decoded)
}

func TestHistoryBaselineRequiresMatchingExactResolution(t *testing.T) {
	root := regapi.Entry{ID: regapi.NewID("test", "root"), Kind: regapi.NamespaceDependency, Registry: regapi.EntryMetadata{Root: true}, Data: payload.New(map[string]any{"component": "test/worker", "version": "1.0.0"})}
	_, err := encodeHistoryBaselineWithResolution(t.Context(), []regapi.Entry{root}, nil)
	require.Error(t, err)
	roots := []regapi.DependencyRoot{{ID: root.ID.String(), Component: "test/worker", Version: "1.0.0"}}
	data, err := json.Marshal(roots)
	require.NoError(t, err)
	resolution := (&regapi.DependencyResolution{InputDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Roots: roots, Modules: []regapi.ResolvedModule{{Name: "test/worker", Version: "1.0.0", Digest: "sha256:fixture"}}}).Canonical()
	data, err = encodeHistoryBaselineWithResolution(t.Context(), []regapi.Entry{root}, resolution)
	require.NoError(t, err)
	var baseline historyv1.Version
	require.NoError(t, protojson.Unmarshal(data, &baseline))
	var graph regapi.DependencyResolution
	require.NoError(t, json.Unmarshal(baseline.Resolution, &graph))
	require.Equal(t, resolution, &graph)
	root.Data = payload.New(map[string]any{"component": "test/worker", "version": "2.0.0"})
	_, err = encodeHistoryBaselineWithResolution(t.Context(), []regapi.Entry{root}, resolution)
	require.Error(t, err)
}
