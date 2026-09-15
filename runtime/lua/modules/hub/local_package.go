// SPDX-License-Identifier: MPL-2.0

package hub

import (
	"io"
	"os"

	lua "github.com/wippyai/go-lua"
	fsmod "github.com/wippyai/runtime/runtime/lua/modules/fs"
	"github.com/wippyai/runtime/runtime/security"
	"github.com/wippyai/wapp"
)

// openLocalPackage uses an already-authorized FS capability, never a host path.
// It owns a separate file handle, so closing the package cannot close a caller's file.
func openLocalPackage(l *lua.LState) int {
	ud := l.CheckUserData(1)
	source, ok := ud.Value.(*fsmod.FS)
	if !ok {
		l.ArgError(1, "fs.FS expected")
		return 0
	}
	path, err := source.Resolve(l.CheckString(2))
	if err != nil {
		return pushError(l, hubCallError(l, err))
	}
	if !security.IsAllowed(l.Context(), "hub.open", path, nil) {
		return pushError(l, permissionDenied(l, "hub.open", path))
	}
	file, err := source.Backend().OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return pushError(l, hubCallError(l, err))
	}
	input, ok := file.(io.ReaderAt)
	if !ok {
		_ = file.Close()
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "package filesystem does not support random access").WithKind(lua.Invalid).WithRetryable(false))
		return 2
	}
	reader, err := wapp.NewReader(input)
	if err != nil {
		_ = file.Close()
		return pushError(l, hubCallError(l, err))
	}
	pushPackageHandle(l, newPackageHandle(l.Context(), file, reader, "", ""))
	l.Push(lua.LNil)
	return 2
}
