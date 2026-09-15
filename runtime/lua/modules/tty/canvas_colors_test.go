// SPDX-License-Identifier: MPL-2.0

package tty

import (
	"image/color"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	lua "github.com/wippyai/go-lua"
)

func TestCanvasDefaultColorsSurviveResetsWithoutReplacingAppColors(t *testing.T) {
	c := &canvasWrapper{width: 12, height: 1, screen: &canvasBuffer{Buffer: uv.NewBuffer(12, 1)}}
	defaults := uv.Style{Fg: color.RGBA{192, 192, 192, 255}, Bg: color.Black}
	c.put(0, 0, "A\x1b[31mB\x1b[0mC\x1b[44mD\x1b[49mE\x1b[38;2;12;34;56mF\x1b[39mG\x1b[7mH", 8, defaults)
	for _, x := range []int{0, 2, 4, 6, 7} {
		require.Equal(t, defaults.Fg, c.screen.CellAt(x, 0).Style.Fg)
		require.Equal(t, defaults.Bg, c.screen.CellAt(x, 0).Style.Bg)
	}
	require.Equal(t, ansi.BasicColor(1), c.screen.CellAt(1, 0).Style.Fg)
	require.Equal(t, ansi.BasicColor(4), c.screen.CellAt(3, 0).Style.Bg)
	require.Equal(t, color.RGBA{12, 34, 56, 255}, c.screen.CellAt(5, 0).Style.Fg)
	require.NotZero(t, c.screen.CellAt(7, 0).Style.Attrs, "reverse video is an application attribute")
	c.put(8, 0, "Z", 1)
	require.Nil(t, c.screen.CellAt(8, 0).Style.Fg)
	require.Nil(t, c.screen.CellAt(8, 0).Style.Bg, "defaults must not leak to the next window")
}

func TestCanvasLuaColorDefaultsValidateBeforeWritingAndPreserveLinks(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	bindTTY(l)
	require.NoError(t, l.DoString(`
        canvas = tty.canvas(8, 2)
        canvas:clear(".")
        canvas:put_rows(1, 1, {"A\27[0mB\27[49mC"}, 3, {foreground = "7", background = "0"})
        canvas:put(4, 1, "\27]8;;https://example.com\27\\L\27]8;;\27\\", 1, {background = "#000000"})
        local ok = pcall(function() canvas:put_rows(1, 2, {"oops"}, 4, {background = "bad-color"}) end)
        assert(not ok)
        assert(canvas:rows()[2] == "........")
    `))
	c := l.GetGlobal("canvas").(*lua.LUserData).Value.(*canvasWrapper)
	for x := range 3 {
		require.Equal(t, ansi.IndexedColor(0), c.screen.CellAt(x, 0).Style.Bg)
		require.Equal(t, ansi.IndexedColor(7), c.screen.CellAt(x, 0).Style.Fg)
	}
	require.Equal(t, "https://example.com", c.screen.CellAt(3, 0).Link.URL)
}
