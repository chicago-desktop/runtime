// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"testing"

	ttyapi "github.com/wippyai/runtime/api/tty"
)

// The rasters the Chicago shell actually sends, at a 190×50 terminal with
// 10×20 cells. Numbers from these benchmarks are the baseline the pixel
// performance work is judged against (chicago-desktop/app#2).
//
//	go test -run '^$' -bench SixelEncode -benchmem ./service/terminal/
const (
	benchCellW  = 10
	benchCellH  = 20
	benchScreen = 190 * benchCellW
)

// win95 is the handful of colours window chrome is drawn with.
var win95 = []color.RGBA{
	{0xc0, 0xc0, 0xc0, 0xff}, {0xff, 0xff, 0xff, 0xff}, {0x80, 0x80, 0x80, 0xff},
	{0x00, 0x00, 0x00, 0xff}, {0x00, 0x00, 0x80, 0xff}, {0x10, 0x84, 0xd0, 0xff},
	{0x00, 0x80, 0x80, 0xff}, {0xdf, 0xdf, 0xdf, 0xff},
}

// titleBar is the head of a 60-column window: bevel lines, a gradient-free
// caption and the three caption buttons.
func titleBar() image.Image {
	width, height := 60*benchCellW, benchCellH
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := win95[4]
			switch {
			case y == 0 || x == 0:
				c = win95[7]
			case y == 1 || x == 1:
				c = win95[1]
			case x >= width-50 && (x-width+50)%16 < 14 && y > 3 && y < 17:
				c = win95[0]
			case x > 24 && x < 200 && y > 6 && y < 14 && (x+y)%3 == 0:
				c = win95[1]
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// tiledStrip is one desktop row of a small tiled wallpaper (Rivets, Weave).
func tiledStrip() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, benchScreen, benchCellH))
	for y := 0; y < benchCellH; y++ {
		for x := 0; x < benchScreen; x++ {
			tx, ty := x%32, y%32
			c := win95[6]
			if (tx-ty)%8 == 0 {
				c = win95[7]
			} else if (tx+ty)%8 == 0 {
				c = win95[3]
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// centeredStrip is one desktop row through a centred many-colour picture
// (Sky): 320 photographic pixels in the middle of a flat desktop colour.
func centeredStrip() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, benchScreen, benchCellH))
	left := (benchScreen - 320) / 2
	for y := 0; y < benchCellH; y++ {
		for x := 0; x < benchScreen; x++ {
			c := win95[6]
			if x >= left && x < left+320 {
				px := x - left
				c = color.RGBA{uint8(40 + px*120/320), uint8(90 + (px*y)%80), uint8(160 + (px+y*7)%90), 0xff}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// reducedStrip is centeredStrip after gfx.image(data, {colors = 256}): the
// Plan 9 palette with error diffusion.
func reducedStrip() image.Image {
	src := centeredStrip()
	paletted := image.NewPaletted(src.Bounds(), palette.Plan9)
	draw.FloydSteinberg.Draw(paletted, paletted.Bounds(), src, image.Point{})
	img := image.NewRGBA(src.Bounds())
	draw.Draw(img, img.Bounds(), paletted, image.Point{}, draw.Src)
	return img
}

// fullScreen is a whole 190×50 desktop of chrome colours.
func fullScreen() image.Image {
	width, height := benchScreen, 50*benchCellH
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, win95[(x/37+y/23)%len(win95)])
		}
	}
	return img
}

func BenchmarkSixelEncode(b *testing.B) {
	probe := ttyapi.NewProbe(func(string) string { return "" })
	probe.SetCellSize(benchCellW, benchCellH)
	cases := []struct {
		name  string
		image image.Image
		row   int
		cols  int
		rows  int
	}{
		{"title-bar-600x20", titleBar(), 5, 60, 1},
		{"tiled-strip-1900x20", tiledStrip(), 7, 190, 1},
		{"centered-strip-1900x20", centeredStrip(), 7, 190, 1},
		{"centered-strip-256-1900x20", reducedStrip(), 7, 190, 1},
		{"full-screen-1900x1000", fullScreen(), 1, 190, 50},
	}
	for _, c := range cases {
		place := ttyapi.Placement{ID: c.name, Image: c.image, Serial: 1, Version: 1, Row: c.row, Col: 1, Cols: c.cols, Rows: c.rows}
		pixels := c.image.Bounds().Dx() * c.image.Bounds().Dy()
		b.Run(c.name, func(b *testing.B) {
			var size int
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				size = len(appendSixel(nil, place, probe))
			}
			b.ReportMetric(float64(size), "bytes/frame")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(pixels), "ns/px")
		})
	}
}
