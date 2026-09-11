package cmd

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/wippyai/runtime/api/registry/history/migration"
)

func TestHistoryMaterializeCommandPublishesOnlyCompleteOutput(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "complete"}[complete], func(t *testing.T) {
			directory := t.TempDir()
			sourcePath := filepath.Join(directory, "source.jsonl")
			outputPath := filepath.Join(directory, "output.jsonl")
			var source bytes.Buffer
			digest := sha256.Sum256(nil)
			writer, err := migration.NewWriter(&source, migration.Header{Format: migration.RawFormat, BaselineDigest: digest[:], Manifest: migration.Manifest{Count: 1}}, 65536)
			require.NoError(t, err)
			if complete {
				require.NoError(t, writer.Write(&migration.Record{}))
				require.NoError(t, writer.Close())
			}
			require.NoError(t, os.WriteFile(sourcePath, source.Bytes(), 0600))
			require.NoError(t, os.WriteFile(outputPath, []byte("existing output"), 0600))
			command := &cobra.Command{}
			command.SetContext(t.Context())
			command.Flags().String("source", sourcePath, "")
			command.Flags().String("output", outputPath, "")
			command.Flags().Int("max-record-bytes", 65536, "")
			err = registryHistoryMaterializeCmd.RunE(command, nil)
			output, readErr := os.ReadFile(outputPath)
			require.NoError(t, readErr)
			if complete {
				require.NoError(t, err)
				reader, err := migration.NewReader(bytes.NewReader(output), 65536)
				require.NoError(t, err)
				require.Equal(t, migration.MaterializedFormat, reader.Header().Format)
			} else {
				require.Error(t, err)
				require.Equal(t, "existing output", string(output))
			}
			entries, err := os.ReadDir(directory)
			require.NoError(t, err)
			require.Len(t, entries, 2)
		})
	}
}
