// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/sixel"
	"github.com/stretchr/testify/require"
)

// decodeSixel turns an encoding back into pixels. The comparison is on
// pixels, not bytes: the fast encoder orders its palette differently and
// stops a colour's line at its last pixel, and neither shows on screen.
//
// charmbracelet's decoder is not used: it loses pixels after some repeat
// introducers (a navy title bar decoded with holes from byte-identical input
// of both encoders), so it cannot be the judge here.
func decodeSixel(t *testing.T, data []byte) image.Image {
	t.Helper()
	s := string(data)
	number := func() int {
		n, i := 0, 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
			i++
		}
		s = s[i:]
		return n
	}
	require.True(t, strings.HasPrefix(s, `"`), "raster attributes first")
	s = s[1:]
	var params []int
	for {
		params = append(params, number())
		if s == "" || s[0] != ';' {
			break
		}
		s = s[1:]
	}
	require.Len(t, params, 4)
	img := image.NewRGBA(image.Rect(0, 0, params[2], params[3]))
	palette := map[int]color.RGBA{}
	var current color.RGBA
	x, band, repeat := 0, 0, 1
	scale := func(v int) uint8 { return uint8((v*255 + 50) / 100) }
	for s != "" {
		c := s[0]
		s = s[1:]
		switch {
		case c == '#':
			index := number()
			if strings.HasPrefix(s, ";2;") {
				s = s[3:]
				r := number()
				s = s[1:]
				g := number()
				s = s[1:]
				b := number()
				palette[index] = color.RGBA{scale(r), scale(g), scale(b), 0xff}
			}
			current = palette[index]
		case c == '!':
			repeat = number()
		case c == '$':
			x = 0
		case c == '-':
			x, band = 0, band+1
		case c >= '?' && c <= '~':
			bits := c - '?'
			for n := 0; n < repeat; n++ {
				for bit := 0; bit < 6; bit++ {
					if bits&(1<<bit) != 0 {
						img.SetRGBA(x, band*6+bit, current)
					}
				}
				x++
			}
			repeat = 1
		default:
			t.Fatalf("unexpected byte %q in sixel data", c)
		}
	}
	return img
}

// sixelUnits is a colour at the precision sixel carries: 0-100 per channel.
func sixelUnits(c color.Color) [3]int {
	r, g, b, _ := c.RGBA()
	return [3]int{int(sixelConvertChannelForTest(r)), int(sixelConvertChannelForTest(g)), int(sixelConvertChannelForTest(b))}
}

func sixelConvertChannelForTest(v uint32) uint32 { return (v + 328) * 100 / 0xffff }

// requireSourcePixels checks a decoding against the picture it came from, at
// sixel precision: a unit of rounding either way, transparent stays empty.
func requireSourcePixels(t *testing.T, source image.Image, pad int, got image.Image) {
	t.Helper()
	b := source.Bounds()
	require.Equal(t, image.Rect(0, 0, b.Dx(), b.Dy()+pad), got.Bounds())
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			want := source.At(b.Min.X+x, b.Min.Y+y)
			_, _, _, wa := want.RGBA()
			_, _, _, ga := got.At(x, y+pad).RGBA()
			if sixelConvertChannelForTest(wa) == 0 {
				require.Zerof(t, ga, "pixel %d,%d must stay transparent", x, y)
				continue
			}
			w, g := sixelUnits(want), sixelUnits(got.At(x, y+pad))
			for i := range w {
				require.InDeltaf(t, w[i], g[i], 1, "pixel %d,%d: want %v, got %v", x, y, w, g)
			}
		}
	}
}

func requireSamePixels(t *testing.T, want, got image.Image) {
	t.Helper()
	require.Equal(t, want.Bounds(), got.Bounds())
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			wr, wg, wb, wa := want.At(x, y).RGBA()
			gr, gg, gb, ga := got.At(x, y).RGBA()
			if wa == 0 && ga == 0 {
				continue
			}
			require.Equalf(t, [4]uint32{wr, wg, wb, wa}, [4]uint32{gr, gg, gb, ga}, "pixel %d,%d", x, y)
		}
	}
}

// withAlpha is chrome with holes: fully transparent corners and one
// translucent line, the two kinds of alpha an icon brings.
func withAlpha() image.Image {
	img := titleBar().(*image.RGBA)
	for y := 0; y < 6; y++ {
		for x := 0; x < 6-y; x++ {
			img.SetRGBA(x, y, color.RGBA{})
		}
	}
	for x := 0; x < img.Bounds().Dx(); x++ {
		img.SetRGBA(x, 10, color.RGBA{0x40, 0x40, 0x40, 0x80})
	}
	return img
}

// fullPalette uses exactly sixelMaxColors colours, scattered so that one band
// holds many of them and a colour's extent differs between rows.
func fullPalette() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 97, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 97; x++ {
			n := (x*7 + y*13) % sixelMaxColors
			img.SetRGBA(x, y, color.RGBA{uint8(n * 4), uint8(255 - n), uint8(n * 9), 0xff})
		}
	}
	return img
}

func TestFastSixelDecodesToTheSamePixels(t *testing.T) {
	sub := fullScreen().(*image.RGBA).SubImage(image.Rect(37, 41, 237, 83))
	cases := map[string]image.Image{
		"title bar":   titleBar(),
		"tiled strip": tiledStrip(),
		"256 colours": fullPalette(),
		"alpha":       withAlpha(),
		"sub-image":   sub,
		"one pixel":   image.NewRGBA(image.Rect(0, 0, 1, 1)),
	}
	for name, img := range cases {
		for pad := 0; pad < 6; pad++ {
			t.Run(fmt.Sprintf("%s/pad-%d", name, pad), func(t *testing.T) {
				var fast bytes.Buffer
				require.True(t, encodeSixelRGBA(&fast, img, pad), name)
				decoded := decodeSixel(t, fast.Bytes())
				requireSourcePixels(t, img, pad, decoded)
				if name == "256 colours" {
					// With the transparent padding the general encoder sees 257
					// colours and quantizes; it is the approximate one there.
					return
				}
				var general bytes.Buffer
				require.NoError(t, (&sixel.Encoder{}).Encode(&general, bandAlignedImage{Image: img, padding: pad}), name)
				requireSamePixels(t, decodeSixel(t, general.Bytes()), decoded)
			})
		}
	}
}

func TestFastSixelLeavesManyColoursToTheGeneralEncoder(t *testing.T) {
	var fast bytes.Buffer
	require.False(t, encodeSixelRGBA(&fast, gradient(64, 64), 0))
	require.Zero(t, fast.Len(), "a refusal writes nothing")

	var viaEntry bytes.Buffer
	require.NoError(t, encodeSixel(&viaEntry, gradient(64, 64), 3))
	require.NotZero(t, viaEntry.Len())
}

func TestFastSixelSkipsOtherImageTypes(t *testing.T) {
	var fast bytes.Buffer
	require.False(t, encodeSixelRGBA(&fast, image.NewNRGBA(image.Rect(0, 0, 4, 4)), 0))
}
