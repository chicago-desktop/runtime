// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	lua "github.com/wippyai/go-lua"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// gfxSupported names the graphics protocol this terminal understands.
//
// It returns nil and a reason rather than false: "not supported" without
// saying what was looked for sends a person to read someone else's
// documentation at random.
//
// The decision itself lives in the tty contract, not here. Two copies of it
// already disagreed once — this side said "no graphics" while the surface
// would have sent sixel happily — and the program believed this side.
//
// It asks about the terminal this process is attached to: a remote session
// has its own, and the one the server was started on is somebody else's.
func gfxSupported(l *lua.LState) int {
	protocol, reason := ttyapi.ProbeFromContext(l.Context()).Detect()
	if protocol != ttyapi.GraphicsNone {
		l.Push(lua.LString(protocol))
		return 1
	}
	l.Push(lua.LNil)
	l.Push(lua.LString(reason))
	return 2
}

// gfxCellSize reports how many pixels one character cell occupies.
//
// A caller drawing the whole screen as one picture needs this and cannot
// derive it: the terminal knows, and only the terminal. Kitty callers can
// ignore it — kitty scales a raster into a rectangle of cells — but a sixel
// raster sized without it lands off the grid, and being off the grid is a
// picture that is subtly the wrong size rather than an error anyone sees.
//
// Returns nil and a reason when nobody asked or the terminal did not answer.
// Guessing eight by sixteen here would be worse than saying nothing: the
// guess is right often enough to look correct and wrong often enough to be
// blamed on the drawing.
func gfxCellSize(l *lua.LState) int {
	width, height, known := ttyapi.ProbeFromContext(l.Context()).CellSize()
	if !known {
		l.Push(lua.LNil)
		l.Push(lua.LString("the terminal did not say how large a cell is " +
			"(asked with CSI 16 t and CSI 14 t at startup)"))
		return 2
	}
	l.Push(lua.LNumber(width))
	l.Push(lua.LNumber(height))
	return 2
}
