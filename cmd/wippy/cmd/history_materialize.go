package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/wippyai/runtime/boot/deps/hub"
)

var registryHistoryMaterializeCmd = &cobra.Command{
	Use:   "materialize-history",
	Short: "Replay legacy history into complete import snapshots",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sourcePath, _ := cmd.Flags().GetString("source")
		outputPath, _ := cmd.Flags().GetString("output")
		maximumBytes, _ := cmd.Flags().GetInt("max-record-bytes")
		if sourcePath == "" || outputPath == "" || maximumBytes <= 0 {
			return errors.New("source, output, and positive max-record-bytes are required")
		}
		sourcePath, err := filepath.Abs(sourcePath)
		if err != nil {
			return err
		}
		outputPath, err = filepath.Abs(outputPath)
		if err != nil {
			return err
		}
		if sourcePath == outputPath {
			return errors.New("source and output paths must differ")
		}
		source, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer source.Close()
		output, err := os.CreateTemp(filepath.Dir(outputPath), ".history-materialized-")
		if err != nil {
			return err
		}
		defer os.Remove(output.Name())
		err = hub.MaterializeHistory(cmd.Context(), source, output, hub.MaterializeHistoryOptions{MaximumRecordBytes: maximumBytes, ScratchDir: filepath.Dir(outputPath)})
		if err == nil {
			err = output.Sync()
		}
		if err := errors.Join(err, output.Close()); err != nil {
			return err
		}
		return os.Rename(output.Name(), outputPath)
	},
}

func init() {
	registryCmd.AddCommand(registryHistoryMaterializeCmd)
	registryHistoryMaterializeCmd.Flags().String("source", "", "raw source bundle path")
	registryHistoryMaterializeCmd.Flags().String("output", "", "complete snapshot bundle path")
	registryHistoryMaterializeCmd.Flags().Int("max-record-bytes", 0, "maximum bytes per bundle record")
}
