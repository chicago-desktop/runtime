// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"sync"

	lua "github.com/wippyai/go-lua"
	"github.com/wippyai/runtime/runtime/lua/engine/value"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Text in pixels.
//
// Without this the pixel path can draw an interface but cannot label it, and
// an unlabeled interface is a picture of one. Everything else here — bevels,
// palettes, exact colors — is in service of a screen a person reads.
//
// The font arrives as BYTES, not as a path. That is a security decision and
// not a style one: reading a file is something the process's own filesystem
// permissions already govern, and a module that opens paths on its own would
// be a way around them. It also means a font can come from anywhere the
// caller can read from — an embedded filesystem, a database row, the network
// — without this module learning about any of those.

const (
	fontTypeName = "gfx.Font"

	// A font is parsed once and held for the life of the object, so the
	// bound is about a mistyped argument rather than about memory: a string
	// this large is not a font, and failing here says so while failing inside
	// the parser says something about table offsets.
	maxFontBytes = 32 << 20

	// Sizes outside this are not text. The upper bound also keeps one glyph's
	// mask from becoming a raster in its own right.
	minFontSize = 1
	maxFontSize = 512
)

// Font is a parsed typeface at one size.
//
// Size belongs to the object rather than to each call because a face carries
// hinting and metrics computed for its size: making one per draw would
// rasterize the same glyph over and over, and per-frame work is exactly what
// this module exists to keep out of Lua.
type Font struct {
	face   font.Face
	name   string
	size   float64
	height int
	ascent int
	mu     sync.Mutex
	smooth bool
}

func init() {
	value.RegisterTypeMethods(nil, fontTypeName,
		map[string]lua.LGoFunc{"__tostring": fontToString},
		map[string]lua.LGoFunc{
			"size":    fontSize,
			"height":  fontHeight,
			"ascent":  fontAscent,
			"measure": fontMeasure,
		})
}

// gfxFontNew parses font bytes at a size. The optional smooth flag supplies
// the default for raster:text; each draw can still override it explicitly.
func gfxFontNew(l *lua.LState) int {
	data := l.CheckString(1)
	if len(data) == 0 {
		l.ArgError(1, "font data is empty")
		return 0
	}
	if len(data) > maxFontBytes {
		l.ArgError(1, "font data is too large to be a font")
		return 0
	}

	size := 12.0
	smooth := false
	if options, ok := l.Get(2).(*lua.LTable); ok && options != nil {
		smooth = optionBool(options, "smooth", false)
		if raw := options.RawGetString("size"); raw != lua.LNil {
			number, ok := numberValue(raw)
			if !ok {
				l.ArgError(2, "font size must be a number")
				return 0
			}
			size = number
		}
	}
	// NaN compares false against both bounds and would sail through to a
	// face whose metrics are 16384 and whose glyph masks are gigabytes.
	if math.IsNaN(size) || size < minFontSize || size > maxFontSize {
		l.ArgError(2, fmt.Sprintf("font size must be between %d and %d", minFontSize, maxFontSize))
		return 0
	}

	parsed, err := sfnt.Parse([]byte(data))
	if err != nil {
		l.ArgError(1, fmt.Sprintf("not a usable font: %v", err))
		return 0
	}

	// Hinting on. At the sizes an interface is drawn — eleven pixels, twelve
	// — an unhinted stem lands between two rows and comes out as two grey
	// ones, which is the blur this whole path exists to avoid.
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		l.ArgError(1, fmt.Sprintf("font cannot be used at size %g: %v", size, err))
		return 0
	}

	name, _ := parsed.Name(nil, sfnt.NameIDFull)
	metrics := face.Metrics()

	value.PushTypedUserData(l, &Font{
		face:   face,
		size:   size,
		name:   name,
		height: metrics.Height.Ceil(),
		ascent: metrics.Ascent.Ceil(),
		smooth: smooth,
	}, fontTypeName)
	return 1
}

func checkFontArg(l *lua.LState, index int) *Font {
	ud, ok := l.Get(index).(*lua.LUserData)
	if !ok {
		l.ArgError(index, "gfx.Font expected")
		return nil
	}
	typed, ok := ud.Value.(*Font)
	if !ok {
		l.ArgError(index, "gfx.Font expected")
		return nil
	}
	return typed
}

func fontToString(l *lua.LState) int {
	f := checkFontArg(l, 1)
	if f == nil {
		return 0
	}
	name := f.name
	if name == "" {
		name = "unnamed"
	}
	l.Push(lua.LString(fmt.Sprintf("gfx.Font(%s, %gpx)", name, f.size)))
	return 1
}

func fontSize(l *lua.LState) int {
	f := checkFontArg(l, 1)
	if f == nil {
		return 0
	}
	l.Push(lua.LNumber(f.size))
	return 1
}

// fontHeight is the distance between baselines: what a caller stacking lines
// needs. It is not the height of any particular glyph.
func fontHeight(l *lua.LState) int {
	f := checkFontArg(l, 1)
	if f == nil {
		return 0
	}
	l.Push(lua.LNumber(f.height))
	return 1
}

// fontAscent is how far the top of the line sits above the baseline. Drawing
// here counts from the top, so this is mostly for a caller aligning text with
// something that is not text.
func fontAscent(l *lua.LState) int {
	f := checkFontArg(l, 1)
	if f == nil {
		return 0
	}
	l.Push(lua.LNumber(f.ascent))
	return 1
}

// fontMeasure returns the width a string would take and the line height.
//
// Measuring is the half of text that makes layout possible — centring a
// label in a button, sizing a menu, deciding where a title stops. A caller
// that cannot measure guesses, and a guess about text width is wrong by a
// different amount in every language.
func fontMeasure(l *lua.LState) int {
	f := checkFontArg(l, 1)
	if f == nil {
		return 0
	}
	text := l.CheckString(2)

	f.mu.Lock()
	width := font.MeasureString(f.face, text)
	f.mu.Unlock()

	l.Push(lua.LNumber(width.Ceil()))
	l.Push(lua.LNumber(f.height))
	return 2
}

// rasterText draws a string with its TOP-LEFT at x, y.
//
// Top-left, not the baseline every font library counts from. Every other call
// in this module places a rectangle by its corner, and one call that secretly
// means something else is how a label ends up sitting on the line above the
// one it belongs to. The baseline is still reachable through ascent().
func rasterText(l *lua.LState) int {
	raster := checkRasterArg(l)
	if raster == nil {
		return 0
	}
	x, okX := integerValue(l.Get(2))
	y, okY := integerValue(l.Get(3))
	if !okX || !okY {
		l.ArgError(2, "text takes two integers: x, y")
		return 0
	}
	text := l.CheckString(4)

	options, ok := l.Get(5).(*lua.LTable)
	if !ok || options == nil {
		l.ArgError(5, "text needs a table with a font and a color")
		return 0
	}
	f := checkFontFromOptions(l, options)
	if f == nil {
		return 0
	}
	paint, err := parseColor(optionString(options, "color", "#000000"))
	if err != nil {
		l.ArgError(5, err.Error())
		return 0
	}

	// Use the face's rendering preference unless this draw overrides it.
	// Bitmap-style art can keep hard edges; small TrueType UI fonts can retain
	// thin strokes without every label repeating its smoothing policy.
	smooth := optionBool(options, "smooth", f.smooth)

	f.mu.Lock()
	advance := drawText(raster, f, x-1, y-1, text, paint, smooth)
	f.mu.Unlock()

	l.Push(lua.LNumber(advance))
	return 1
}

// drawText lays the string out and returns the width actually advanced.
//
// Returning the advance rather than nothing lets a caller draw a run in
// pieces — a label with one letter underlined, a line with a word in bold —
// without measuring each piece separately and hoping the two agree.
func drawText(raster *Raster, f *Font, x, y int, text string, paint color.RGBA, smooth bool) int {
	origin := x
	// The baseline. Glyphs are placed against it, and the caller's y is the
	// top of the line.
	dot := fixed.P(x, y+f.ascent)
	source := image.NewUniform(paint)
	drew := false

	var previous rune
	for index, current := range text {
		if index > 0 {
			dot.X += f.face.Kern(previous, current)
		}
		bounds, mask, maskPoint, advance, ok := f.face.Glyph(dot, current)
		if !ok {
			// The font has nothing for this rune, not even a box. Advancing
			// by the space width keeps the rest of the line where it belongs:
			// a missing glyph should cost one gap, not shift the sentence.
			dot.X += spaceAdvance(f)
			previous = current
			continue
		}
		if smooth {
			draw.DrawMask(raster.img, bounds, source, image.Point{}, mask, maskPoint, draw.Over)
			drew = true
		} else if blitThreshold(raster, bounds, mask, maskPoint, paint) {
			drew = true
		}
		dot.X += advance
		previous = current
	}

	if drew {
		raster.version++
	}
	return dot.X.Ceil() - origin
}

// blitThreshold writes the glyph as solid pixels wherever the mask covers at
// least half of one, and reports whether anything landed on the raster.
//
// Half is the threshold that keeps a stem one pixel wide. Lower and every
// glyph grows a fringe; higher and thin strokes disappear at small sizes.
func blitThreshold(raster *Raster, bounds image.Rectangle, mask image.Image, maskPoint image.Point, paint color.RGBA) bool {
	clipped := bounds.Intersect(raster.img.Bounds())
	if clipped.Empty() {
		return false
	}
	drew := false
	for row := clipped.Min.Y; row < clipped.Max.Y; row++ {
		for column := clipped.Min.X; column < clipped.Max.X; column++ {
			_, _, _, alpha := mask.At(
				maskPoint.X+column-bounds.Min.X,
				maskPoint.Y+row-bounds.Min.Y,
			).RGBA()
			if alpha < 0x8000 {
				continue
			}
			offset := raster.img.PixOffset(column, row)
			raster.img.Pix[offset] = paint.R
			raster.img.Pix[offset+1] = paint.G
			raster.img.Pix[offset+2] = paint.B
			raster.img.Pix[offset+3] = paint.A
			drew = true
		}
	}
	return drew
}

func spaceAdvance(f *Font) fixed.Int26_6 {
	if advance, ok := f.face.GlyphAdvance(' '); ok {
		return advance
	}
	return fixed.I(f.height / 2)
}

func checkFontFromOptions(l *lua.LState, options *lua.LTable) *Font {
	raw := options.RawGetString("font")
	ud, ok := raw.(*lua.LUserData)
	if !ok {
		l.ArgError(5, "text needs a gfx.Font in the font field")
		return nil
	}
	typed, ok := ud.Value.(*Font)
	if !ok {
		l.ArgError(5, "text needs a gfx.Font in the font field")
		return nil
	}
	return typed
}

func optionString(options *lua.LTable, key, fallback string) string {
	if text, ok := options.RawGetString(key).(lua.LString); ok {
		return string(text)
	}
	return fallback
}

func optionBool(options *lua.LTable, key string, fallback bool) bool {
	switch value := options.RawGetString(key).(type) {
	case lua.LBool:
		return bool(value)
	case *lua.LNilType:
		return fallback
	}
	return fallback
}

// numberValue accepts both Lua number shapes, for the same reason
// integerValue does: a literal and the result of arithmetic are different
// types, and taking only one makes half the callers wrong.
func numberValue(v lua.LValue) (float64, bool) {
	switch value := v.(type) {
	case lua.LInteger:
		return float64(value), true
	case lua.LNumber:
		return float64(value), true
	}
	return 0, false
}
