// SPDX-License-Identifier: MPL-2.0

package registry

import "context"

// Preview describes dependency-expanded changes against one effective state.
// Changes includes both durable and derived operations in application order;
// History contains the subset recorded in durable history. It carries no live
// resources and does not authorize activation or promise handler validation.
type Preview struct {
	Version    Version
	History    ChangeSet
	Resolution *DependencyResolution
	Digest     string
	Changes    ChangeSet
	Revision   uint64
}

// SnapshotPreviewer uses the same dependency expansion as publication without
// preparing effects, dispatching transitions, or writing history. Expansion may
// download verified artifacts into the native cache. Staged resources must be
// released before return. Callers must still bind reviewed external selections
// to publication; a state revision alone does not pin an external catalog.
type SnapshotPreviewer interface {
	PreviewAt(context.Context, uint64, ChangeSet) (*Preview, error)
}

// PreviewWriter re-expands and checks the reviewed measurement before preparing
// effects. It rejects both registry drift and changed external selections.
type PreviewWriter interface {
	ApplyPreview(context.Context, uint64, string, ChangeSet) (Version, error)
}
