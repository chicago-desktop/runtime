// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"fmt"
	"github.com/wippyai/runtime/runtime/lua/modules/gfx"
	"sync"

	lua "github.com/wippyai/go-lua"
	ttyapi "github.com/wippyai/runtime/api/tty"
	"github.com/wippyai/runtime/runtime/lua/engine/value"
)

const viewportTypeName = "tty.Viewport"

type viewportWrapper struct {
	view     ttyapi.Viewport
	updates  *updateBridge
	closeErr error
	once     sync.Once
}

func init() {
	value.RegisterTypeMethods(nil, viewportTypeName,
		map[string]lua.LGoFunc{"__gc": viewportGC, "__tostring": viewportToString},
		map[string]lua.LGoFunc{
			"grant": viewportGrant, "handle": viewportHandle,
			"snapshot": viewportSnapshot, "updates": viewportUpdates, "send": viewportSend,
			"resize": viewportResize, "close": viewportClose,
			"terminal": viewportTerminal,
		})
}

func ttyAttach(l *lua.LState) int {
	service := ttyapi.GetService(l.Context())
	if service == nil {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "tty service unavailable").WithKind(lua.Unavailable).WithRetryable(false))
		return 2
	}
	view, err := service.Attach(l.Context(), l.CheckString(1))
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "attach viewport"))
		return 2
	}
	if view == nil {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "attached viewport is unavailable").
			WithKind(lua.Internal).WithRetryable(false))
		return 2
	}
	pushViewport(l, view)
	return 2
}

func ttyViewportNew(l *lua.LState) int {
	service := ttyapi.GetService(l.Context())
	if service == nil {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "tty service unavailable").WithKind(lua.Unavailable).WithRetryable(false))
		return 2
	}
	width, height := 80, 24
	if options := l.OptTable(1, nil); options != nil {
		var err error
		if width, err = viewportDimension(options, "width", width); err != nil {
			return invalidArgument(l, err.Error())
		}
		if height, err = viewportDimension(options, "height", height); err != nil {
			return invalidArgument(l, err.Error())
		}
	}
	if err := ttyapi.ValidateViewportSize(width, height); err != nil {
		return invalidArgument(l, err.Error())
	}
	view, err := service.Create(l.Context(), width, height)
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "create viewport"))
		return 2
	}
	if view == nil {
		l.Push(lua.LNil)
		l.Push(lua.NewLuaError(l, "created viewport is unavailable").
			WithKind(lua.Internal).WithRetryable(false))
		return 2
	}
	pushViewport(l, view)
	return 2
}

func pushViewport(l *lua.LState, view ttyapi.Viewport) {
	value.PushTypedUserData(l, &viewportWrapper{view: view}, viewportTypeName)
	l.Push(lua.LNil)
}

func viewportDimension(options *lua.LTable, field string, defaultValue int) (int, error) {
	value := options.RawGetString(field)
	if value == lua.LNil {
		return defaultValue, nil
	}
	return viewportDimensionValue(value, field)
}

func viewportDimensionValue(value lua.LValue, field string) (int, error) {
	dimension, ok := integerValue(value)
	if !ok || dimension < 1 || dimension > maxTerminalDimension {
		return 0, fmt.Errorf("viewport %s must be an integer between 1 and %d", field, maxTerminalDimension)
	}
	return dimension, nil
}

func checkViewport(l *lua.LState) *viewportWrapper {
	ud := l.CheckUserData(1)
	if v, ok := ud.Value.(*viewportWrapper); ok {
		return v
	}
	l.ArgError(1, "tty.Viewport expected")
	return nil
}

func viewportToString(l *lua.LState) int { l.Push(lua.LString("tty.Viewport{}")); return 1 }

func viewportGrant(l *lua.LState) int {
	grant := checkViewport(l).view.Grant()
	if grant == "" {
		return invalidArgument(l, "viewport has no producer grant")
	}
	l.Push(lua.LString(grant))
	l.Push(lua.LNil)
	return 2
}

func viewportHandle(l *lua.LState) int {
	l.Push(lua.LString(checkViewport(l).view.Handle()))
	return 1
}

func viewportSnapshot(l *lua.LState) int {
	s := checkViewport(l).view.Snapshot()
	if l.GetTop() >= 2 {
		after, ok := integerValue(l.Get(2))
		if !ok {
			l.ArgError(2, "viewport revision must be an integer")
			return 0
		}
		if after >= 0 && uint64(after) == s.Revision {
			l.Push(lua.LNil)
			return 1
		}
	}
	rows := l.CreateTable(len(s.Rows), 0)
	for i, row := range s.Rows {
		rows.RawSetInt(i+1, lua.LString(row))
	}
	result := l.CreateTable(0, 4)
	result.RawSetString("revision", lua.LInteger(s.Revision))
	result.RawSetString("width", lua.LInteger(s.Width))
	result.RawSetString("height", lua.LInteger(s.Height))
	result.RawSetString("rows", rows)
	// The pictures standing on the screen, with the identity each arrived
	// with: a viewer that carries them somewhere else recognises a picture by
	// its serial and version, and sends the pixels only for one it has not
	// seen. Coordinates are one-based here, as everything Lua-facing is.
	if len(s.Placements) > 0 {
		images := l.CreateTable(len(s.Placements), 0)
		for i, placement := range s.Placements {
			entry := l.CreateTable(0, 8)
			entry.RawSetString("id", lua.LString(placement.ID))
			entry.RawSetString("x", lua.LInteger(placement.Col))
			entry.RawSetString("y", lua.LInteger(placement.Row))
			entry.RawSetString("cols", lua.LInteger(placement.Cols))
			entry.RawSetString("rows", lua.LInteger(placement.Rows))
			entry.RawSetString("z", lua.LInteger(placement.Z))
			entry.RawSetString("version", lua.LInteger(int(placement.Version)))
			entry.RawSetString("serial", lua.LInteger(int(placement.Serial)))
			gfx.PushRaster(l, gfx.Adopt(placement.Image, placement.Version, placement.Serial))
			entry.RawSetString("raster", l.Get(-1))
			l.Pop(1)
			images.RawSetInt(i+1, entry)
		}
		result.RawSetString("images", images)
	}
	if s.Cursor != nil {
		cursor := l.CreateTable(0, 3)
		cursor.RawSetString("x", lua.LInteger(s.Cursor.Column+1))
		cursor.RawSetString("y", lua.LInteger(s.Cursor.Row+1))
		cursor.RawSetString("visible", lua.LBool(s.Cursor.Visible))
		cursor.Immutable = true
		result.RawSetString("cursor", cursor)
	}
	l.Push(result)
	return 1
}

func viewportUpdates(l *lua.LState) int {
	v := checkViewport(l)
	if v.updates == nil {
		bridge, err := newUpdateBridge(l, v.view)
		if err != nil {
			l.Push(lua.LNil)
			l.Push(lua.WrapErrorWithLua(l, err, "subscribe viewport updates"))
			return 2
		}
		v.updates = bridge
	}
	l.Push(v.updates.value)
	l.Push(lua.LNil)
	return 2
}

func viewportSend(l *lua.LState) int {
	v := checkViewport(l)
	event, err := DecodeEvent(l.CheckTable(2))
	if err != nil {
		return invalidArgument(l, err.Error())
	}
	if err := v.view.Send(event); err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "send viewport event"))
		return 2
	}
	l.Push(lua.LTrue)
	l.Push(lua.LNil)
	return 2
}

func viewportResize(l *lua.LState) int {
	v := checkViewport(l)
	width, err := viewportDimensionValue(l.Get(2), "width")
	if err != nil {
		return invalidArgument(l, err.Error())
	}
	height, err := viewportDimensionValue(l.Get(3), "height")
	if err != nil {
		return invalidArgument(l, err.Error())
	}
	if err := ttyapi.ValidateViewportSize(width, height); err != nil {
		return invalidArgument(l, err.Error())
	}
	err = v.view.Resize(width, height)
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "resize viewport"))
		return 2
	}
	l.Push(lua.LTrue)
	l.Push(lua.LNil)
	return 2
}

func viewportClose(l *lua.LState) int {
	v := checkViewport(l)
	v.once.Do(func() {
		if v.updates != nil {
			v.updates.close()
		}
		v.closeErr = v.view.Close()
	})
	if v.closeErr != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, v.closeErr, "close viewport"))
		return 2
	}
	l.Push(lua.LTrue)
	l.Push(lua.LNil)
	return 2
}

func viewportGC(l *lua.LState) int { _ = viewportClose(l); l.Pop(2); return 0 }

// viewportTerminal is `view:terminal(protocol, cell_width?, cell_height?)`.
//
// The viewer says which screen it is really showing this viewport on. Only
// the viewer can know: the producer runs where the viewport was made, and on
// another node that is not even the same machine. Without it a nested
// desktop asks whether it may draw pictures, hears nothing, and draws in
// cells for good.
//
// Call it BEFORE starting the producer. A producer chooses how to draw the
// first time it asks, and a terminal described afterwards only takes effect
// the next time it asks again.
func viewportTerminal(l *lua.LState) int {
	v := checkViewport(l)
	terminal, ok := v.view.(ttyapi.ViewportTerminal)
	if !ok {
		return invalidArgument(l, "this viewport cannot be told which terminal it is shown on")
	}
	protocol := l.CheckString(2)
	cellWidth, cellHeight := 0, 0
	if l.Get(3) != lua.LNil {
		value, ok := integerValue(l.Get(3))
		if !ok || value < 0 {
			return invalidArgument(l, "cell_width must be a non-negative integer")
		}
		cellWidth = value
	}
	if l.Get(4) != lua.LNil {
		value, ok := integerValue(l.Get(4))
		if !ok || value < 0 {
			return invalidArgument(l, "cell_height must be a non-negative integer")
		}
		cellHeight = value
	}
	if err := terminal.SetTerminal(protocol, cellWidth, cellHeight); err != nil {
		l.Push(lua.LNil)
		l.Push(lua.WrapErrorWithLua(l, err, "describe the viewport's terminal"))
		return 2
	}
	l.Push(lua.LTrue)
	l.Push(lua.LNil)
	return 2
}
