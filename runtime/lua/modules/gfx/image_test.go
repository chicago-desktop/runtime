// SPDX-License-Identifier: MPL-2.0

package gfx

import (
	"image"
	"image/color"
	"testing"
)

func iconWithHole(t *testing.T) *Raster {
	t.Helper()
	// A four-pixel picture: two opaque, two transparent. Small enough that
	// every pixel can be asserted, which is what makes the alpha question
	// answerable rather than plausible.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	img.SetRGBA(1, 1, color.RGBA{B: 255, A: 255})
	return &Raster{img: img, version: 1}
}

func TestBlitHonoursTransparency(t *testing.T) {
	// An icon is a picture with a hole in it. A blit that ignored alpha would
	// paint the hole and put a rectangle on the desktop — which looks like a
	// badly drawn icon, not like a compositing bug.
	target := testRaster(4, 4)
	teal := color.RGBA{G: 0x80, B: 0x80, A: 255}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			target.img.SetRGBA(x, y, teal)
		}
	}
	source := iconWithHole(t)

	blitInto(target, source, 2, 2)

	if got := target.img.RGBAAt(1, 1); got.R != 255 || got.A != 255 {
		t.Fatalf("opaque pixel did not land: %+v", got)
	}
	if got := target.img.RGBAAt(2, 1); got != teal {
		t.Fatalf("the hole was painted over: %+v", got)
	}
	if got := target.img.RGBAAt(2, 2); got.B != 255 {
		t.Fatalf("second opaque pixel did not land: %+v", got)
	}
}

func TestBlitOffTheEdgeChangesNothing(t *testing.T) {
	// An icon at the border is ordinary. What must NOT happen is a version
	// bump with no pixels moved: that retransmits the same picture every
	// frame, turning a still image into a flickering one.
	target := testRaster(4, 4)
	before := target.version

	blitInto(target, iconWithHole(t), 50, 50)

	if target.version != before {
		t.Fatal("a blit that painted nothing must not change the version")
	}
}

func TestBlitClipsAtTheBorder(t *testing.T) {
	target := testRaster(4, 4)
	source := iconWithHole(t)

	blitInto(target, source, 4, 4) // half on, half off

	if got := target.img.RGBAAt(3, 3); got.R != 255 || got.A != 255 {
		t.Fatalf("the visible half did not land: %+v", got)
	}
	if target.version == 1 {
		t.Fatal("a blit that painted something must change the version")
	}
}

func TestRotationMovesWholePixels(t *testing.T) {
	// Right angles only, and they must move pixels rather than resample them:
	// a pixel interface that resamples stops being one. The corner is asserted
	// because a rotation that transposed but did not mirror looks plausible in
	// a screenshot and is wrong everywhere it matters.
	source := &Raster{img: image.NewRGBA(image.Rect(0, 0, 3, 2)), version: 1}
	mark := color.RGBA{R: 255, A: 255}
	source.img.SetRGBA(0, 0, mark) // top-left

	turned := rotated(source, 90)
	if got := turned.img.Bounds(); got.Dx() != 2 || got.Dy() != 3 {
		t.Fatalf("90° must swap the sides: got %v", got)
	}
	// Clockwise: the top-left corner lands top-right.
	if got := turned.img.RGBAAt(1, 0); got != mark {
		t.Fatalf("top-left should land top-right, found %+v", got)
	}

	back := rotated(rotated(rotated(turned, 90), 90), 90)
	if back.img.RGBAAt(0, 0) != mark {
		t.Fatal("four turns must come back to where it started")
	}
	if rotated(source, 0) != source {
		t.Fatal("no rotation must not copy")
	}
}
