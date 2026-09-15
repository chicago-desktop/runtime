// SPDX-License-Identifier: MPL-2.0

package hub

import (
	"context"

	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/boot/deps/artifact"
)

var _ regapi.FinalizingEffect = (*artifact.Effect)(nil)

// buildArtifactEffect binds derived artifact outputs to the same transaction as
// the dependency graph change. Module selection and verification remain owned
// by DependencyHandler; the artifact subsystem only sees exact WAPP paths.
func (h *DependencyHandler) buildArtifactEffect(
	ctx context.Context,
	resolved []ResolvedModule,
	state regapi.State,
) (regapi.Effect, error) {
	if h == nil || h.artifacts == nil {
		return nil, nil
	}

	packs := make([]artifact.WAPP, 0, len(resolved))
	directoryVersions := make(map[string]string)
	directoryRoots := make(map[string]string)
	seen := make(map[string]struct{}, len(resolved))
	for _, module := range resolved {
		moduleName := module.Org + "/" + module.Name
		if root, replaced := h.replacementPath(moduleName); replaced ||
			module.Source == moduleSourceReplacementTreeV1 {
			directoryVersions[moduleName] = module.Version
			if replaced {
				directoryRoots[moduleName] = root
			}
			continue
		}
		path, err := h.ensureModuleAvailable(ctx, module)
		if err != nil {
			return nil, err
		}
		if module.Source == moduleSourceGit {
			// A git checkout is a source directory, never an archive: its
			// declared resources resolve against the tree as a replacement's do.
			directoryVersions[moduleName] = module.Version
			directoryRoots[moduleName] = path
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		packs = append(packs, artifact.WAPP{
			Path:          path,
			ModuleVersion: module.Version,
		})
	}
	var resources []artifact.Resource
	if len(directoryVersions) > 0 {
		var err error
		resources, err = artifact.DirectoryResources(ctx, state, directoryRoots, directoryVersions)
		if err != nil {
			return nil, err
		}
	}
	return artifact.NewEffect(h.artifacts, packs, resources, h.artifactRoot)
}
