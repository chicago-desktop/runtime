// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"

	lua "github.com/wippyai/go-lua"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// The Go font travels as a byte slice inside x/image, so these tests depend
// on nothing installed on the machine running them. A test that loaded a
// system font would pass or fail by which machine ran it, and the failure
// would look like a bug in the rasteriser.
func testFont(t *testing.T, size float64) *Font {
	t.Helper()
	parsed, err := sfnt.Parse(goregular.TTF)
	if err != nil {
		t.Fatalf("parsing the test font: %v", err)
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
		Size: size, DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		t.Fatalf("building a face: %v", err)
	}
	metrics := face.Metrics()
	return &Font{
		face:   face,
		size:   size,
		height: metrics.Height.Ceil(),
		ascent: metrics.Ascent.Ceil(),
	}
}

func testRaster(width, height int) *Raster {
	return &Raster{img: image.NewRGBA(image.Rect(0, 0, width, height)), version: 1}
}

func countPainted(raster *Raster) int {
	painted := 0
	for offset := 3; offset < len(raster.img.Pix); offset += 4 {
		if raster.img.Pix[offset] != 0 {
			painted++
		}
	}
	return painted
}

var black = color.RGBA{A: 255}

func TestLuaFontSmoothingDefaultsAndDrawOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, faceOptions, drawOptions string
		wantSmooth                     bool
	}{
		{"legacy default", "", "", false},
		{"smooth face", ", smooth = true", "", true},
		{"crisp override", ", smooth = true", ", smooth = false", false},
		{"smooth override", ", smooth = false", ", smooth = true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := lua.NewState()
			defer l.Close()
			mod, _ := buildModule()
			l.SetGlobal("gfx", mod)
			l.SetGlobal("font_bytes", lua.LString(goregular.TTF))
			if err := l.DoString(fmt.Sprintf(`
				local face = gfx.font(font_bytes, {size = 13%s})
				local other = gfx.font(font_bytes, {size = 13, smooth = false})
				assert(face:height() == other:height())
				assert(face:measure("Мой компьютер") == other:measure("Мой компьютер"))
				painted = gfx.raster(200, 30)
				painted:fill("#c0c0c0")
				painted:text(3, 3, "Мой компьютер", {font = face, color = "#000000"%s})
			`, tc.faceOptions, tc.drawOptions)); err != nil {
				t.Fatal(err)
			}
			raster := CheckRaster(l.GetGlobal("painted"))
			mixed, ink := false, false
			for offset := 0; offset < len(raster.img.Pix); offset += 4 {
				r, g, b, a := raster.img.Pix[offset], raster.img.Pix[offset+1], raster.img.Pix[offset+2], raster.img.Pix[offset+3]
				if a != 255 || r != g || g != b || r > 192 {
					t.Fatalf("text must blend into its actual gray background without a white halo: %d %d %d %d", r, g, b, a)
				}
				ink = ink || r < 192
				mixed = mixed || (r > 0 && r < 192)
			}
			if !ink || mixed != tc.wantSmooth {
				t.Fatalf("ink=%v blended edges=%v, want blended edges=%v", ink, mixed, tc.wantSmooth)
			}
		})
	}
}

func TestTextPutsPixelsOnTheRaster(t *testing.T) {
	f := testFont(t, 14)
	raster := testRaster(120, 30)

	advance := drawText(raster, f, 0, 0, "Window", black, false)

	if painted := countPainted(raster); painted == 0 {
		t.Fatal("drawing text painted nothing")
	}
	if advance <= 0 {
		t.Fatalf("advance should be the width drawn, got %d", advance)
	}
	if raster.version == 1 {
		t.Fatal("drawing must change the version, or the surface never resends the picture")
	}
}

func TestCyrillicRenders(t *testing.T) {
	// The interface this exists for is written in Russian. A rasteriser that
	// silently draws nothing for Cyrillic would pass every other test here
	// and fail the only screen that matters.
	f := testFont(t, 14)
	latin := testRaster(160, 30)
	cyrillic := testRaster(160, 30)

	drawText(latin, f, 0, 0, "Computer", black, false)
	drawText(cyrillic, f, 0, 0, "Компьютер", black, false)

	if countPainted(cyrillic) == 0 {
		t.Fatal("Cyrillic painted nothing")
	}
	// Not a pixel-for-pixel comparison — different letters. The point is that
	// one is not a fraction of the other, which is what a font falling back
	// to blanks would look like.
	if countPainted(cyrillic)*3 < countPainted(latin) {
		t.Fatalf("Cyrillic painted %d pixels against %d for Latin — looks like missing glyphs",
			countPainted(cyrillic), countPainted(latin))
	}
}

func TestCrispTextHasNoGreyPixels(t *testing.T) {
	// The whole reason to leave the cell grid is a one-pixel bevel. If a
	// glyph comes out with a soft fringe, text drawn beside that bevel looks
	// like a different interface.
	f := testFont(t, 12)
	raster := testRaster(120, 24)
	drawText(raster, f, 0, 0, "Programs", black, false)

	for offset := 0; offset < len(raster.img.Pix); offset += 4 {
		alpha := raster.img.Pix[offset+3]
		if alpha != 0 && alpha != 255 {
			t.Fatalf("crisp text produced a partly transparent pixel (alpha %d)", alpha)
		}
		red, green, blue := raster.img.Pix[offset], raster.img.Pix[offset+1], raster.img.Pix[offset+2]
		if alpha == 255 && (red != 0 || green != 0 || blue != 0) {
			t.Fatalf("crisp text produced a blended colour (%d,%d,%d)", red, green, blue)
		}
	}
}

func TestSmoothTextIsActuallySmooth(t *testing.T) {
	// The control for the test above: if the threshold were applied in both
	// modes, that test would pass while the option did nothing.
	f := testFont(t, 24)
	raster := testRaster(200, 40)
	drawText(raster, f, 0, 0, "Programs", black, true)

	for offset := 3; offset < len(raster.img.Pix); offset += 4 {
		if alpha := raster.img.Pix[offset]; alpha != 0 && alpha != 255 {
			return
		}
	}
	t.Fatal("smoothing produced no partly covered pixels — the option did nothing")
}

func TestTextOffTheEdgeIsClippedNotFatal(t *testing.T) {
	// A label longer than its window, a title at the last row. Ordinary, and
	// refusing it would push clipping arithmetic into every caller.
	f := testFont(t, 14)
	raster := testRaster(40, 20)

	drawText(raster, f, -30, -10, "Свойства системы", black, false)
	drawText(raster, f, 35, 15, "Свойства системы", black, false)
	drawText(raster, f, 500, 500, "далеко", black, false)
}

func TestNothingDrawnLeavesTheVersionAlone(t *testing.T) {
	// The surface resends a picture when its version moves. A draw entirely
	// off the raster that still bumped the version would retransmit the same
	// pixels every frame — a still picture that flickers.
	f := testFont(t, 14)
	raster := testRaster(40, 20)
	before := raster.version

	drawText(raster, f, 500, 500, "далеко", black, false)

	if raster.version != before {
		t.Fatal("a draw that painted nothing must not change the version")
	}
}

func TestMissingGlyphCostsOneGapNotTheSentence(t *testing.T) {
	// A rune the font has nothing for must not collapse the rest of the line
	// onto itself: the words after it would silently move.
	f := testFont(t, 14)
	raster := testRaster(200, 30)

	plain := drawText(raster, f, 0, 0, "ab", black, false)
	withMissing := drawText(testRaster(200, 30), f, 0, 0, "a\U0001F600b", black, false)

	if withMissing <= plain {
		t.Fatalf("a missing glyph should still advance: %d against %d", withMissing, plain)
	}
}

func TestMeasureMatchesWhatGetsDrawn(t *testing.T) {
	// Layout is done by measuring and drawing is done separately. If the two
	// disagree, every centred label is off by an amount that changes with the
	// text — the hardest kind of visual bug to attribute.
	f := testFont(t, 13)
	const text = "Панель управления"

	measured := font.MeasureString(f.face, text).Ceil()
	drawn := drawText(testRaster(400, 30), f, 0, 0, text, black, false)

	if difference := measured - drawn; difference > 2 || difference < -2 {
		t.Fatalf("measure says %d, drawing advanced %d", measured, drawn)
	}
}

func TestTextIsPlacedFromTheTopNotTheBaseline(t *testing.T) {
	// Every other call in this module places a rectangle by its corner. A
	// text call that secretly meant the baseline would put a label a whole
	// ascent higher than the caller asked, which reads as the wrong row.
	f := testFont(t, 16)
	raster := testRaster(120, 60)

	drawText(raster, f, 0, 20, "Hg", black, false)

	topmost := -1
	for row := 0; row < 60 && topmost < 0; row++ {
		for column := 0; column < 120; column++ {
			if raster.img.Pix[raster.img.PixOffset(column, row)+3] != 0 {
				topmost = row
				break
			}
		}
	}
	if topmost < 0 {
		t.Fatal("nothing was drawn")
	}
	if topmost < 20 {
		t.Fatalf("text reached above the y it was given: topmost row %d", topmost)
	}
	if topmost > 20+f.ascent {
		t.Fatalf("text starts %d rows below the y it was given", topmost-20)
	}
}

func TestEncodedRasterIsAReadablePNG(t *testing.T) {
	// The point of encoding is that a person can open the file. A byte slice
	// that only this process can read would satisfy a test that checked the
	// length and nothing else.
	f := testFont(t, 14)
	raster := testRaster(120, 40)
	raster.img.Set(0, 0, color.RGBA{R: 0xc0, G: 0xc0, B: 0xc0, A: 0xff})
	drawText(raster, f, 4, 4, "Свойства", black, false)

	var buf bytes.Buffer
	if err := png.Encode(&buf, raster.img); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("what was written back is not a png: %v", err)
	}
	if decoded.Bounds() != raster.img.Bounds() {
		t.Fatalf("size changed in the round trip: %v against %v",
			decoded.Bounds(), raster.img.Bounds())
	}
	// And the pixels survived — an encoder that wrote a blank image of the
	// right size would pass everything above.
	painted := 0
	bounds := decoded.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, alpha := decoded.At(x, y).RGBA(); alpha != 0 {
				painted++
			}
		}
	}
	if painted == 0 {
		t.Fatal("the encoded image is empty")
	}
}
