// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	lua "github.com/wippyai/go-lua"
	"github.com/wippyai/runtime/runtime/lua/engine/value"
)

const (
	rasterTypeName = "gfx.Raster"

	// A raster is held in memory as four bytes per pixel and shipped through
	// a pty. The bound is deliberately far below what a terminal would accept
	// so a mistyped size fails as an argument error rather than as an
	// allocation the process never comes back from.
	maxRasterSide   = 8192
	maxRasterPixels = 1 << 24
)

// Raster is a pixel buffer owned by Lua and drawn into from Go.
//
// Version changes on every write. It is what lets the terminal surface tell a
// picture that moved from one that only got asked about again: retransmitting
// an unchanged raster every frame is the difference between a still picture
// and a flickering one.
type Raster struct {
	img     *image.RGBA
	version uint64

	// A number unique to this buffer for the life of the process.
	//
	// Version alone cannot identify a picture. A caller that builds a fresh
	// raster every frame restarts the count at one, and drawing it with the
	// same number of calls lands on the same number again — so two DIFFERENT
	// pictures arrive carrying identical versions, and the surface, seeing no
	// change, does not send the second. The clock stops on screen and nothing
	// reports a fault.
	//
	// Identity settles it: a new buffer is a new picture, whatever its
	// version says.
	serial uint64
}

// rasterSerials hands out identities. Wrapping after 2^64 rasters is not a
// case worth code.
var rasterSerials atomic.Uint64

// Image returns the pixels. The surface encodes them; nothing else reads it.
func (r *Raster) Image() image.Image { return r.img }

// Version reports the current revision of the pixels.
func (r *Raster) Version() uint64 { return r.version }

// Serial identifies the buffer itself, as opposed to its contents.
func (r *Raster) Serial() uint64 { return r.serial }

// Bounds reports the size in pixels.
func (r *Raster) Bounds() image.Rectangle { return r.img.Bounds() }

func init() {
	value.RegisterTypeMethods(nil, rasterTypeName,
		map[string]lua.LGoFunc{"__tostring": rasterToString},
		map[string]lua.LGoFunc{
			"size":    rasterSize,
			"version": rasterVersion,
			"fill":    rasterFill,
			"rect":    rasterRect,
			"set":     rasterSet,
			"text":    rasterText,
			"encode":  rasterEncode,
			"blit":    rasterBlit,
			"scaled":  rasterScaled,
		})
}

// CheckRaster returns the raster behind a Lua value, or nil when the value is
// something else. It is exported because the terminal surface has to reach a
// raster to put it on screen, and reaching it here is cheaper than teaching
// tty about pixels.
func CheckRaster(v lua.LValue) *Raster {
	ud, ok := v.(*lua.LUserData)
	if !ok {
		return nil
	}
	raster, ok := ud.Value.(*Raster)
	if !ok {
		return nil
	}
	return raster
}

func gfxRasterNew(l *lua.LState) int {
	width, ok := integerValue(l.Get(1))
	if !ok {
		l.ArgError(1, "raster width must be an integer")
		return 0
	}
	height, ok := integerValue(l.Get(2))
	if !ok {
		l.ArgError(2, "raster height must be an integer")
		return 0
	}
	if width < 1 || width > maxRasterSide {
		l.ArgError(1, "raster width must be positive and bounded")
		return 0
	}
	if height < 1 || height > maxRasterSide {
		l.ArgError(2, "raster height must be positive and bounded")
		return 0
	}
	if width*height > maxRasterPixels {
		l.ArgError(1, "raster is too large")
		return 0
	}

	raster := newRaster(image.NewRGBA(image.Rect(0, 0, width, height)))
	value.PushTypedUserData(l, raster, rasterTypeName)
	return 1
}

// newRaster wraps pixels and gives them an identity.
func newRaster(img *image.RGBA) *Raster {
	return &Raster{img: img, version: 1, serial: rasterSerials.Add(1)}
}

func checkRasterArg(l *lua.LState) *Raster {
	ud := l.CheckUserData(1)
	if raster, ok := ud.Value.(*Raster); ok {
		return raster
	}
	l.ArgError(1, "gfx.Raster expected")
	return nil
}

func rasterToString(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	size := raster.img.Bounds().Size()
	l.Push(lua.LString(fmt.Sprintf("gfx.Raster(%dx%d)", size.X, size.Y)))
	return 1
}

func rasterSize(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	size := raster.img.Bounds().Size()
	l.Push(lua.LNumber(size.X))
	l.Push(lua.LNumber(size.Y))
	return 2
}

func rasterVersion(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	l.Push(lua.LNumber(raster.version))
	return 1
}

func rasterFill(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	paint, err := parseColor(l.CheckString(2))
	if err != nil {
		l.ArgError(2, err.Error())
		return 0
	}
	bounds := raster.img.Bounds()
	fillRect(raster, bounds.Min.X, bounds.Min.Y, bounds.Dx(), bounds.Dy(), paint)
	return 0
}

// rasterRect fills a rectangle. Coordinates are one-based, like everything
// else a Lua process here counts: mixing origins inside one program is the
// kind of difference nobody notices until a border is one pixel off.
func rasterRect(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	x, okX := integerValue(l.Get(2))
	y, okY := integerValue(l.Get(3))
	w, okW := integerValue(l.Get(4))
	h, okH := integerValue(l.Get(5))
	if !okX || !okY || !okW || !okH {
		l.ArgError(2, "rect takes four integers: x, y, width, height")
		return 0
	}
	paint, err := parseColor(l.CheckString(6))
	if err != nil {
		l.ArgError(6, err.Error())
		return 0
	}
	fillRect(raster, x-1, y-1, w, h, paint)
	return 0
}

func rasterSet(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	x, okX := integerValue(l.Get(2))
	y, okY := integerValue(l.Get(3))
	if !okX || !okY {
		l.ArgError(2, "set takes two integers: x, y")
		return 0
	}
	paint, err := parseColor(l.CheckString(4))
	if err != nil {
		l.ArgError(4, err.Error())
		return 0
	}
	fillRect(raster, x-1, y-1, 1, 1, paint)
	return 0
}

// fillRect clips to the raster and never fails. A rectangle drawn partly off
// the edge is ordinary — a window at the border, a bevel at the last row —
// and refusing it would push clipping arithmetic into every caller.
func fillRect(raster *Raster, x, y, w, h int, paint color.RGBA) {
	bounds := raster.img.Bounds()
	left, top := max(x, bounds.Min.X), max(y, bounds.Min.Y)
	right, bottom := min(x+w, bounds.Max.X), min(y+h, bounds.Max.Y)
	if left >= right || top >= bottom {
		return
	}
	for row := top; row < bottom; row++ {
		offset := raster.img.PixOffset(left, row)
		for column := left; column < right; column++ {
			raster.img.Pix[offset] = paint.R
			raster.img.Pix[offset+1] = paint.G
			raster.img.Pix[offset+2] = paint.B
			raster.img.Pix[offset+3] = paint.A
			offset += 4
		}
	}
	raster.version++
}

// parseColor takes "#rgb", "#rrggbb" or "#rrggbbaa".
//
// Names are deliberately absent: the palette this is drawn for is exact
// values, and a name that resolves differently on two terminals is the thing
// being escaped by moving to pixels at all.
func parseColor(text string) (color.RGBA, error) {
	value := strings.TrimPrefix(strings.TrimSpace(text), "#")
	switch len(value) {
	case 3:
		value = string([]byte{value[0], value[0], value[1], value[1], value[2], value[2], 'f', 'f'})
	case 6:
		value += "ff"
	case 8:
	default:
		return color.RGBA{}, fmt.Errorf("color must be #rgb, #rrggbb or #rrggbbaa, got %q", text)
	}
	parts := [4]uint8{}
	for index := 0; index < 4; index++ {
		component, err := strconv.ParseUint(value[index*2:index*2+2], 16, 8)
		if err != nil {
			return color.RGBA{}, fmt.Errorf("color %q is not hexadecimal", text)
		}
		parts[index] = uint8(component)
	}
	return color.RGBA{R: parts[0], G: parts[1], B: parts[2], A: parts[3]}, nil
}

// integerValue accepts both Lua number shapes. A literal arrives as
// LInteger, arithmetic produces LNumber, and taking only one of them makes
// gfx.raster(480, 240) fail while gfx.raster(480.0, 240.0) works — a
// difference nobody would guess from the message.
func integerValue(v lua.LValue) (int, bool) {
	var number float64
	switch value := v.(type) {
	case lua.LInteger:
		number = float64(value)
	case lua.LNumber:
		number = float64(value)
	default:
		return 0, false
	}
	if math.Trunc(number) != number || number > float64(math.MaxInt) || number < float64(math.MinInt) {
		return 0, false
	}
	return int(number), true
}

// rasterEncode returns the pixels as an image file.
//
// This exists so a picture can be LOOKED AT without a terminal that shows
// pictures. A theme drawn in pixels is otherwise checkable only on the stand
// and only by a person's eye — which is exactly the cycle that leaving the
// cell grid was supposed to end.
//
// PNG only, and named rather than assumed: a second format later should be a
// new argument, not a different meaning for the same call.
func rasterEncode(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	format := "png"
	if given, ok := l.Get(2).(lua.LString); ok {
		format = strings.ToLower(strings.TrimSpace(string(given)))
	}
	if format != "png" {
		l.ArgError(2, "only png is supported, got "+format)
		return 0
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, raster.img); err != nil {
		l.Push(lua.LNil)
		l.Push(lua.LString(err.Error()))
		return 2
	}
	l.Push(lua.LString(buf.String()))
	return 1
}
