// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"fmt"
	xdraw "golang.org/x/image/draw"
	"image"
	"image/draw"
	"strings"

	// Registered for image.Decode. PNG is what an icon arrives as; the other
	// two cost nothing and save a caller from converting a file by hand only
	// to find out the format was never the problem.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	lua "github.com/wippyai/go-lua"
	"github.com/wippyai/runtime/runtime/lua/engine/value"
)

// Pictures that were drawn by someone else.
//
// Primitives can build a bevel but not an icon: the Windows 95 "My Computer"
// is a 32×32 raster somebody drew pixel by pixel in 1994, and no amount of
// rectangles reproduces it. So a raster can be decoded from bytes and stamped
// into another one.
//
// Bytes rather than a path, for the same reason fonts are — reading a file is
// governed by the process's own filesystem permissions, and a module that
// opened paths would be a way around them.

// maxImageBytes bounds the encoded form. The decoded size is checked from
// the header BEFORE decoding — see gfxImageNew — because the decoders
// allocate the whole picture from the header first.
const maxImageBytes = 64 << 20

// gfxImageNew decodes an image into a raster.
func gfxImageNew(l *lua.LState) int {
	data := l.CheckString(1)
	if len(data) == 0 {
		l.ArgError(1, "image data is empty")
		return 0
	}
	if len(data) > maxImageBytes {
		l.ArgError(1, "image data is too large")
		return 0
	}

	// The header first, the pixels second. image/png allocates the whole
	// picture from IHDR before it has read a byte of data, and GIF and JPEG
	// do the same from theirs; a 72-byte file claiming 30000x30000 would
	// take 3.6 GB on the way to a size check that never runs. The refusal
	// has to come from the header alone.
	config, format, err := image.DecodeConfig(strings.NewReader(data))
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.LString(fmt.Sprintf("not a usable image: %v", err)))
		return 2
	}
	if config.Width < 1 || config.Height < 1 {
		l.Push(lua.LNil)
		l.Push(lua.LString("image has no pixels"))
		return 2
	}
	// The same bounds a raster made by hand has to satisfy.
	if config.Width > maxRasterSide || config.Height > maxRasterSide ||
		config.Width*config.Height > maxRasterPixels {
		l.Push(lua.LNil)
		l.Push(lua.LString(fmt.Sprintf(
			"%s is %dx%d, past what a raster may be", format, config.Width, config.Height)))
		return 2
	}

	decoded, _, err := image.Decode(strings.NewReader(data))
	if err != nil {
		l.Push(lua.LNil)
		l.Push(lua.LString(fmt.Sprintf("not a usable image: %v", err)))
		return 2
	}

	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 1 || height < 1 || width > maxRasterSide || height > maxRasterSide ||
		width*height > maxRasterPixels {
		// The header lied. Rare, but a header is a claim, not a proof.
		l.Push(lua.LNil)
		l.Push(lua.LString(fmt.Sprintf(
			"decoded %s is %dx%d, not what its header said", format, width, height)))
		return 2
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), decoded, bounds.Min, draw.Src)

	value.PushTypedUserData(l, newRaster(img), rasterTypeName)
	return 1
}

// rasterBlit stamps one raster into another with its top-left at x, y.
//
// Transparency is honoured, because that is the whole point: an icon is a
// picture with a hole in it, and a blit that ignored alpha would paint the
// hole as black and put a rectangle on the desktop.
func rasterBlit(l *lua.LState) int {
	target := checkRasterArg(l)
	if target == nil {
		return 0
	}
	source := CheckRaster(l.Get(2))
	if source == nil {
		l.ArgError(2, "gfx.Raster expected")
		return 0
	}
	if source == target {
		// Overlapping copy into itself reads pixels it has already written.
		// Refusing is better than the streaks that produces.
		l.ArgError(2, "a raster cannot be drawn into itself")
		return 0
	}
	x, okX := integerValue(l.Get(3))
	y, okY := integerValue(l.Get(4))
	if !okX || !okY {
		l.ArgError(3, "blit takes two integers: x, y")
		return 0
	}

	// Rotation, because some things in this interface are written sideways
	// and a glyph rasteriser draws only one way. The Windows 95 Start menu
	// has its name running bottom-to-top down the left edge; without this a
	// caller would have to ship the banner as a picture, and the text in it
	// would stop matching the text everywhere else.
	rotate := 0
	if options, ok := l.Get(5).(*lua.LTable); ok && options != nil {
		if raw := options.RawGetString("rotate"); raw != lua.LNil {
			degrees, ok := integerValue(raw)
			if !ok {
				l.ArgError(5, "rotate must be a number")
				return 0
			}
			switch ((degrees % 360) + 360) % 360 {
			case 0:
				rotate = 0
			case 90:
				rotate = 90
			case 180:
				rotate = 180
			case 270:
				rotate = 270
			default:
				// Only right angles. Anything else needs resampling, and a
				// resampled pixel interface stops being one.
				l.ArgError(5, "rotate must be 0, 90, 180 or 270")
				return 0
			}
		}
	}

	blitInto(target, rotated(source, rotate), x, y)
	return 0
}

// rotated returns the source turned by a right angle, or the source itself
// when there is nothing to turn. Right angles only: they move whole pixels,
// and a pixel interface that resamples stops being one.
func rotated(source *Raster, degrees int) *Raster {
	if degrees == 0 {
		return source
	}
	bounds := source.img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	turned := image.NewRGBA(image.Rect(0, 0, width, height))
	if degrees == 90 || degrees == 270 {
		turned = image.NewRGBA(image.Rect(0, 0, height, width))
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := source.img.RGBAAt(bounds.Min.X+x, bounds.Min.Y+y)
			switch degrees {
			case 90:
				// Clockwise: the top row becomes the right column.
				turned.SetRGBA(height-1-y, x, pixel)
			case 180:
				turned.SetRGBA(width-1-x, height-1-y, pixel)
			default: // 270
				turned.SetRGBA(y, width-1-x, pixel)
			}
		}
	}
	// Not newRaster: this is a temporary, and giving it an identity would
	// hand out serial numbers for pictures nobody keeps.
	return &Raster{img: turned, version: source.version}
}

// blitInto is the operation itself, apart from the Lua plumbing, so that it
// can be asserted pixel by pixel.
func blitInto(target, source *Raster, x, y int) {
	// One-based, like every other coordinate here.
	at := image.Pt(x-1, y-1)
	area := image.Rectangle{Min: at, Max: at.Add(source.img.Bounds().Size())}
	clipped := area.Intersect(target.img.Bounds())
	if clipped.Empty() {
		// Off the edge entirely. Not an error — an icon at the border is
		// ordinary — and NOT a version change: a version that moved without
		// pixels moving retransmits the same picture every frame.
		return
	}

	draw.Draw(target.img, clipped, source.img, source.img.Bounds().Min.Add(clipped.Min.Sub(at)), draw.Over)
	target.version++
}

// rasterScaled returns a NEW raster with the picture resampled to width x
// height. It is the one place in gfx that resamples, and it is a separate
// raster on purpose: the interface stays pixel-exact, and a caller that wants
// a photograph to fit a window asks for that explicitly and keeps the result.
//
// Nearest neighbour by default — it keeps 16-colour artwork hard-edged, and a
// downscaled photograph is still recognisable. `smooth = true` switches to
// bilinear for photographs where blockiness would be worse than blur.
func rasterScaled(l *lua.LState) int {
	source := checkRasterArg(l)
	if source == nil {
		return 0
	}
	width, okW := integerValue(l.Get(2))
	height, okH := integerValue(l.Get(3))
	if !okW || !okH {
		l.ArgError(2, "scaled takes two integers: width, height")
		return 0
	}
	if width < 1 || height < 1 || width > maxRasterSide || height > maxRasterSide ||
		width*height > maxRasterPixels {
		l.ArgError(2, "scaled size must be positive and bounded")
		return 0
	}
	smooth := false
	if options, ok := l.Get(4).(*lua.LTable); ok && options != nil {
		if raw := options.RawGetString("smooth"); raw != lua.LNil {
			smooth = lua.LVAsBool(raw)
		}
	}

	scaled := image.NewRGBA(image.Rect(0, 0, width, height))
	var scaler xdraw.Scaler = xdraw.NearestNeighbor
	if smooth {
		scaler = xdraw.BiLinear
	}
	scaler.Scale(scaled, scaled.Bounds(), source.img, source.img.Bounds(), xdraw.Src, nil)
	value.PushTypedUserData(l, newRaster(scaled), rasterTypeName)
	return 1
}
