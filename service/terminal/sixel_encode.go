// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"image"
	"strconv"
	"sync"

	"github.com/charmbracelet/x/ansi/sixel"
)

// A sixel encoder for the rasters the desktop actually sends.
//
// charmbracelet's encoder is general: it reads every pixel twice through
// image.At, quantizes a palette with median cut even when the picture has a
// dozen colours, and keeps one bitset for the whole image sized bands × width
// × 6 × colours. Window chrome, icons and wallpaper strips are *image.RGBA
// with at most a few hundred colours, and for them all of that is waste:
// measured at 90-430 ns and one allocation per pixel (BenchmarkSixelEncode).
//
// This one reads Pix directly, takes the exact palette, and works one six-row
// band at a time. The output is the same format — raster attributes, palette,
// bands — and decodes to the same pixels; the golden tests hold it to that.
// Anything it does not handle (another image type, more than sixelMaxColors
// distinct colours) returns false and goes to the general encoder.

// sixelMaxColors is the palette size terminals are expected to honour.
const sixelMaxColors = 256

// sixelChannel converts one premultiplied 8-bit channel to sixel's 0-100
// range the way the general encoder does (from 16 bits, rounded), so both
// paths put the same numbers in the palette.
var sixelChannel = func() (table [256]uint8) {
	for v := range table {
		table[v] = uint8((uint32(v)*0x101 + 328) * 100 / 0xffff)
	}
	return table
}()

// sixelScratch is reused between encodings; the desktop encodes on one
// goroutine per surface, but surfaces are many.
type sixelScratch struct {
	masks   []uint8 // six-bit column masks, colours × width
	lookup  map[uint32]uint16
	palette []uint32
	minX    []int
	maxX    []int
	used    []uint16
}

var sixelScratchPool = sync.Pool{New: func() any {
	return &sixelScratch{lookup: make(map[uint32]uint16, sixelMaxColors)}
}}

// encodeSixelRGBA writes raster attributes, palette and pixel data for img,
// with padTop transparent rows above it (screen-origin sixel aligns the
// picture with the six-row band grid that way). It reports false, having
// written nothing, when the image is not one it handles.
func encodeSixelRGBA(w *bytes.Buffer, img image.Image, padTop int) bool {
	rgba, ok := img.(*image.RGBA)
	if !ok {
		return false
	}
	bounds := rgba.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return false
	}

	s := sixelScratchPool.Get().(*sixelScratch)
	defer sixelScratchPool.Put(s)
	clear(s.lookup)
	s.palette = s.palette[:0]

	// Pass one: the exact palette, in sixel units. A colour that converts to
	// zero alpha is transparent and never drawn, as in the general encoder.
	const transparent = 0xffff
	var last uint32
	haveLast := false
	for y := 0; y < height; y++ {
		row := rgba.Pix[rgba.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
		for x := 0; x < width; x++ {
			p := row[x*4 : x*4+4 : x*4+4]
			key := uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
			if haveLast && key == last {
				continue
			}
			last, haveLast = key, true
			if _, seen := s.lookup[key]; seen {
				continue
			}
			if sixelChannel[p[3]] == 0 {
				s.lookup[key] = transparent
				continue
			}
			sixel := uint32(sixelChannel[p[0]])<<16 | uint32(sixelChannel[p[1]])<<8 | uint32(sixelChannel[p[2]])
			index := -1
			for i, known := range s.palette {
				if known == sixel {
					index = i
					break
				}
			}
			if index < 0 {
				if len(s.palette) == sixelMaxColors {
					return false
				}
				index = len(s.palette)
				s.palette = append(s.palette, sixel)
			}
			s.lookup[key] = uint16(index)
		}
	}
	colors := len(s.palette)

	fullHeight := height + padTop
	w.WriteString(`"1;1;`)
	w.WriteString(strconv.Itoa(width))
	w.WriteByte(';')
	w.WriteString(strconv.Itoa(fullHeight))
	for i, c := range s.palette {
		w.WriteByte('#')
		w.WriteString(strconv.Itoa(i))
		w.WriteString(";2;")
		w.WriteString(strconv.Itoa(int(c >> 16)))
		w.WriteByte(';')
		w.WriteString(strconv.Itoa(int(c >> 8 & 0xff)))
		w.WriteByte(';')
		w.WriteString(strconv.Itoa(int(c & 0xff)))
	}

	if cap(s.masks) < colors*width {
		s.masks = make([]uint8, colors*width)
	}
	s.masks = s.masks[:colors*width]
	clear(s.masks)
	s.minX = grow(s.minX, colors)
	s.maxX = grow(s.maxX, colors)
	for i := range colors {
		s.minX[i], s.maxX[i] = -1, -1
	}

	bands := (fullHeight + 5) / 6
	for band := 0; band < bands; band++ {
		if band > 0 {
			w.WriteByte('-')
		}
		s.used = s.used[:0]
		for bit := 0; bit < 6; bit++ {
			y := band*6 + bit - padTop
			if y < 0 || y >= height {
				continue
			}
			row := rgba.Pix[rgba.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
			var lastIndex uint16
			haveLast = false
			for x := 0; x < width; x++ {
				p := row[x*4 : x*4+4 : x*4+4]
				key := uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				if !haveLast || key != last {
					last, haveLast = key, true
					lastIndex = s.lookup[key]
				}
				if lastIndex == transparent {
					continue
				}
				c := int(lastIndex)
				// A colour's extent spans all six rows of the band, and each row
				// starts again from the left.
				switch {
				case s.minX[c] < 0:
					s.minX[c], s.maxX[c] = x, x
					s.used = append(s.used, lastIndex)
				case x < s.minX[c]:
					s.minX[c] = x
				case x > s.maxX[c]:
					s.maxX[c] = x
				}
				s.masks[c*width+x] |= 1 << bit
			}
		}

		for n, c16 := range s.used {
			c := int(c16)
			if n > 0 {
				w.WriteByte('$')
			}
			w.WriteByte('#')
			w.WriteString(strconv.Itoa(c))
			line := s.masks[c*width : c*width+width]
			from, to := s.minX[c], s.maxX[c]
			writeSixelRun(w, from, '?')
			run, runLen := line[from], 0
			for x := from; x <= to; x++ {
				if line[x] == run {
					runLen++
					continue
				}
				writeSixelRun(w, runLen, run+'?')
				run, runLen = line[x], 1
			}
			writeSixelRun(w, runLen, run+'?')
			clear(line[from : to+1])
			s.minX[c], s.maxX[c] = -1, -1
		}
	}
	w.WriteByte('-')
	return true
}

func writeSixelRun(w *bytes.Buffer, count int, char byte) {
	switch {
	case count <= 0:
	case count > 3:
		w.WriteByte('!')
		w.WriteString(strconv.Itoa(count))
		w.WriteByte(char)
	default:
		for range count {
			w.WriteByte(char)
		}
	}
}

func grow(s []int, n int) []int {
	if cap(s) < n {
		return make([]int, n)
	}
	return s[:n]
}

// encodeSixel is the one entry point: the fast path when it applies, the
// general encoder otherwise.
func encodeSixel(w *bytes.Buffer, img image.Image, padTop int) error {
	if encodeSixelRGBA(w, img, padTop) {
		return nil
	}
	// The wrapper also rebases a sub-image to the origin, which the general
	// encoder assumes.
	return (&sixel.Encoder{}).Encode(w, bandAlignedImage{Image: img, padding: padTop})
}
