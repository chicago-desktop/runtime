// SPDX-License-Identifier: MPL-2.0

package registry

import (
	lua "github.com/wippyai/go-lua"
	regapi "github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/runtime/security"
)

func changesPreview(l *lua.LState) int {
	changes := checkChanges(l)
	if changes == nil {
		return 0
	}
	changes.hasPreview, changes.previewDigest = true, ""
	if !security.IsAllowed(l.Context(), "registry.preview", "", nil) {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "registry preview is not allowed").WithKind(lua.PermissionDenied).WithRetryable(false))
		return 2
	}
	previewer, ok := changes.snapshot.reg.(regapi.SnapshotPreviewer)
	if !ok || changes.snapshot.revision == 0 || changes.snapshot.overlayOwner != "" || len(changes.ops) == 0 {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "preview requires changes from a current durable registry snapshot").WithKind(lua.Invalid).WithRetryable(false))
		return 2
	}
	preview, err := previewer.PreviewAt(l.Context(), changes.snapshot.revision, changes.ops)
	if err != nil {
		l.Push(lua.LNil)
		l.Push(wrapRegistryError(l, err, "preview changes"))
		return 2
	}
	result := l.CreateTable(0, 4)
	result.RawSetString("digest", lua.LString(preview.Digest))
	for name, operations := range map[string]regapi.ChangeSet{"changes": preview.Changes, "history": preview.History} {
		items := l.CreateTable(len(operations), 0)
		for i, op := range operations {
			if !security.IsAllowed(l.Context(), "registry.get", op.Entry.ID.String(), nil) {
				l.Push(lua.LNil)
				l.Push(lua.NewLuaError(l, "not allowed to read preview entry: "+op.Entry.ID.String()).WithKind(lua.PermissionDenied).WithRetryable(false))
				return 2
			}
			entry, err := stateEntryToLuaTable(l, op.Entry)
			if err != nil {
				l.Push(lua.LNil)
				l.Push(wrapRegistryError(l, err, "decode preview entry"))
				return 2
			}
			item := l.CreateTable(0, 2)
			item.RawSetString("kind", lua.LString(op.Kind))
			item.RawSetString("entry", entry)
			items.RawSetInt(i+1, item)
		}
		result.RawSetString(name, items)
	}
	if preview.Resolution != nil {
		result.RawSetString("resolution", resolutionToLuaTable(l, preview.Resolution))
	}
	changes.previewDigest = preview.Digest
	l.Push(result)
	l.Push(lua.LNil)
	return 2
}
