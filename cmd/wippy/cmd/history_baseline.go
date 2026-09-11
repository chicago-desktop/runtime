package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"
	regapi "github.com/wippyai/runtime/api/registry"
	entryencoding "github.com/wippyai/runtime/api/registry/history/encoding"
	historyv1 "github.com/wippyai/runtime/api/registry/history/v1"
	"github.com/wippyai/runtime/boot/deps/hub"
	syspayload "github.com/wippyai/runtime/system/payload"
	jsonpayload "github.com/wippyai/runtime/system/payload/json"
	msgpackpayload "github.com/wippyai/runtime/system/payload/msgpack"
	yamlpayload "github.com/wippyai/runtime/system/payload/yaml"
	"github.com/wippyai/runtime/system/registry/topology"
	"google.golang.org/protobuf/encoding/protojson"
)

var registryHistoryBaselineCmd = &cobra.Command{
	Use:   "export-history-baseline",
	Short: "Export the deployment baseline for history import",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		silentLogs = true
		lockFile, _ := cmd.Flags().GetString("lock-file")
		entries, err := loadRegistryEntries(cmd, lockFile)
		if err != nil {
			return err
		}
		resolutionPath, _ := cmd.Flags().GetString("resolution-file")
		resolution, err := readHistoryResolution(resolutionPath)
		if err != nil {
			return err
		}
		data, err := encodeHistoryBaselineWithResolution(cmd.Context(), entries, resolution)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(append(data, '\n'))
		return err
	},
}

func init() {
	registryCmd.AddCommand(registryHistoryBaselineCmd)
	registryHistoryBaselineCmd.Flags().StringP("lock-file", "l", defaultLockFile, "path to lock file")
	registryHistoryBaselineCmd.Flags().String("resolution-file", "", "exact dependency resolution JSON for the baseline")
}

func readHistoryResolution(path string) (*regapi.DependencyResolution, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var resolution regapi.DependencyResolution
	if err := decoder.Decode(&resolution); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("resolution file must contain one JSON object")
	}
	if !resolution.Valid() {
		return nil, regapi.ErrInvalidDependencyResolution
	}
	return resolution.Canonical(), nil
}

func encodeHistoryBaseline(entries []regapi.Entry) ([]byte, error) {
	return encodeHistoryBaselineWithResolution(context.Background(), entries, nil)
}

func encodeHistoryBaselineWithResolution(ctx context.Context, entries []regapi.Entry, resolution *regapi.DependencyResolution) ([]byte, error) {
	if err := topology.ValidateUniqueEntryIDs("baseline", entries); err != nil {
		return nil, err
	}
	transcoder := syspayload.NewTranscoder()
	jsonpayload.Register(transcoder)
	yamlpayload.Register(transcoder)
	msgpackpayload.Register(transcoder)
	if err := hub.ValidateDependencyResolution(ctx, entries, resolution, transcoder); err != nil {
		return nil, err
	}
	baseline := &historyv1.Version{Entries: make([]*historyv1.Mutation, len(entries))}
	if resolution != nil {
		data, err := json.Marshal(resolution.Canonical())
		if err != nil {
			return nil, err
		}
		baseline.Resolution = data
	}
	for i, entry := range entries {
		data, err := entryencoding.EncodeEntry(entry)
		if err != nil {
			return nil, err
		}
		id := entry.ID.Canonical()
		baseline.Entries[i] = &historyv1.Mutation{EntryId: id.String(), Value: data}
	}
	sort.Slice(baseline.Entries, func(i, j int) bool { return baseline.Entries[i].EntryId < baseline.Entries[j].EntryId })
	return (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(baseline)
}
