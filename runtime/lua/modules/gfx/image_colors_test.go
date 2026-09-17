// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	lua "github.com/wippyai/go-lua"
)

func pngBytes(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// photo has far more than 256 colours, like the Sky wallpaper.
func photo(alpha uint8) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 2), uint8(y * 3), uint8(x + y), alpha})
		}
	}
	return img
}

func runImage(t *testing.T, data, call string) (*lua.LState, error) {
	t.Helper()
	l := lua.NewState()
	mod, _ := buildModule()
	l.SetGlobal("gfx", mod)
	l.SetGlobal("data", lua.LString(data))
	return l, l.DoString(call)
}

func distinctColors(img *image.RGBA) int {
	seen := map[color.RGBA]bool{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			seen[img.RGBAAt(x, y)] = true
		}
	}
	return len(seen)
}

func TestImageColorsReducesAPhotoOnce(t *testing.T) {
	data := pngBytes(t, photo(0xff))

	l, err := runImage(t, data, `full = gfx.image(data); reduced, why = gfx.image(data, {colors = 256})`)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	full, reduced := CheckRaster(l.GetGlobal("full")), CheckRaster(l.GetGlobal("reduced"))
	if reduced == nil {
		t.Fatalf("no raster: %v", l.GetGlobal("why"))
	}
	if n := distinctColors(full.img); n <= reducedColors {
		t.Fatalf("the fixture must need reducing, has %d colours", n)
	}
	if n := distinctColors(reduced.img); n > reducedColors {
		t.Fatalf("reduced picture has %d colours", n)
	}
	if reduced.img.Bounds() != full.img.Bounds() {
		t.Fatalf("size changed: %v vs %v", reduced.img.Bounds(), full.img.Bounds())
	}
	// Dithering keeps the average: a reduced picture that went dark or
	// lost a channel would still pass the colour count.
	average := func(img *image.RGBA) (r, g, b int) {
		for i := 0; i < len(img.Pix); i += 4 {
			r, g, b = r+int(img.Pix[i]), g+int(img.Pix[i+1]), b+int(img.Pix[i+2])
		}
		n := len(img.Pix) / 4
		return r / n, g / n, b / n
	}
	fr, fg, fb := average(full.img)
	rr, rg, rb := average(reduced.img)
	for _, d := range []int{fr - rr, fg - rg, fb - rb} {
		if d < -8 || d > 8 {
			t.Fatalf("average colour drifted: %d,%d,%d vs %d,%d,%d", fr, fg, fb, rr, rg, rb)
		}
	}
}

func TestImageColorsRefusesTransparency(t *testing.T) {
	l, err := runImage(t, pngBytes(t, photo(0x80)), `reduced, why = gfx.image(data, {colors = 256})`)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.GetGlobal("reduced") != lua.LNil {
		t.Fatal("a translucent picture must not be reduced")
	}
	if why := l.GetGlobal("why").String(); !strings.Contains(why, "transparency") {
		t.Fatalf("the refusal must say why, got %q", why)
	}
}

func TestImageColorsTakesOnly256(t *testing.T) {
	l, err := runImage(t, pngBytes(t, photo(0xff)), `gfx.image(data, {colors = 16})`)
	defer l.Close()
	if err == nil || !strings.Contains(err.Error(), "colors must be 256") {
		t.Fatalf("want an argument error, got %v", err)
	}
}
