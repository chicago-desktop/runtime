// SPDX-License-Identifier: MPL-2.0

// Package gfx gives Lua a pixel buffer for terminals that can show rasters.
//
// The split with tty is deliberate and worth stating once: tty owns the
// terminal — the lease, the cursor, the diffing of text rows — and gfx owns
// pixels. A raster made here reaches the screen by riding in a tty frame, so
// there is still exactly one writer to the terminal and one frame
// transaction. Drawing that happens per frame — filling, blitting, later
// rasterising glyphs — belongs in Go; doing it from Lua on every frame is
// what makes a picture cost more than it is worth.
package gfx

import (
	lua "github.com/wippyai/go-lua"
	luaapi "github.com/wippyai/runtime/api/runtime/lua"
)

// Module is the gfx module definition.
var Module = &luaapi.ModuleDef{
	Name:        "gfx",
	Description: "Pixel rasters for terminals that can display graphics",
	Class:       []string{luaapi.ClassIO},
	Build:       buildModule,
	Types:       ModuleTypes,
}

func buildModule() (*lua.LTable, []luaapi.YieldType) {
	mod := lua.CreateTable(0, 4)
	mod.RawSetString("supported", lua.LGoFunc(gfxSupported))
	mod.RawSetString("raster", lua.LGoFunc(gfxRasterNew))
	mod.RawSetString("font", lua.LGoFunc(gfxFontNew))
	mod.RawSetString("cell_size", lua.LGoFunc(gfxCellSize))
	mod.RawSetString("image", lua.LGoFunc(gfxImageNew))
	return mod, nil
}
