package hub

import (
	"context"
	"fmt"

	"github.com/wippyai/runtime/api/payload"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/system/registry/topology"
)

func ValidateDependencyResolution(ctx context.Context, snapshot regapi.State, resolution *regapi.DependencyResolution, transcoder payload.Transcoder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := topology.ValidateUniqueEntryIDs("dependency snapshot", snapshot); err != nil {
		return err
	}
	if resolution == nil {
		for _, entry := range snapshot {
			if isRootDependency(entry) {
				return fmt.Errorf("dependency root %s has no resolution", entry.ID.String())
			}
		}
		return nil
	}
	if !resolution.Valid() {
		return regapi.ErrInvalidDependencyResolution
	}
	if transcoder == nil {
		return ErrDependencyTranscoderMissing
	}
	handler := &DependencyHandler{}
	roots, _, err := handler.collectResolutionDependencies(ctx, snapshot, transcoder, resolution.Roots, resolution.References)
	if err != nil {
		return err
	}
	if dependencyInputDigest(roots) != resolution.InputDigest {
		return fmt.Errorf("dependency input digest does not match the published declarations")
	}
	return nil
}
