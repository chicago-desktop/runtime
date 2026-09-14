// SPDX-License-Identifier: MPL-2.0

package toml

import (
	"github.com/wippyai/go-lua/types/io"
	"github.com/wippyai/go-lua/types/typ"
)

// ModuleTypes describes the toml Lua surface.
func ModuleTypes() *io.Manifest {
	manifest := io.NewManifest("toml")
	manifest.SetExport(typ.NewInterface("toml", []typ.Method{
		{Name: "insert", Type: typ.Func().
			Param("document", typ.String).
			Param("path", typ.NewArray(typ.String)).
			Param("source", typ.String).
			Returns(typ.String, typ.NewOptional(typ.LuaError)).
			Build()},
	}))
	return manifest
}
