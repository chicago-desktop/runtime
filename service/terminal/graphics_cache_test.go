// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

// encoderProbe counts encodings and keeps the bytes of the last one. The
// count is the evidence: "the same bytes went out" would also hold for a
// surface that encoded the same picture twice.
type encoderProbe struct {
	last  []byte
	calls int
}

func probedSurface(out *bytes.Buffer, protocol graphicsProtocol) (*Surface, *encoderProbe) {
	surface := NewSurface(out, ttyapi.SurfaceOptions{})
	surface.graphics = protocol
	probe := &encoderProbe{}
	surface.encode = func(dst []byte, p graphicsProtocol, place ttyapi.Placement) []byte {
		probe.calls++
		start := len(dst)
		dst = appendPlace(dst, p, place)
		probe.last = append([]byte(nil), dst[start:]...)
		return dst
	}
	surface.encodeSixel = func(place ttyapi.Placement, pad int) []byte {
		probe.calls++
		probe.last = sixelPayload(place.Image, pad)
		return probe.last
	}
	return surface, probe
}

// gradient is a picture with many colors, so the sixel palette is real work.
func gradient(width, height int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: uint8((x + y) % 256), A: 0xff,
			})
		}
	}
	return img
}

// client is one window-sized raster on rows 2..4; row 3 carries text.
func client(img image.Image, version uint64, row int) []ttyapi.Placement {
	return []ttyapi.Placement{{ID: "client", Image: img, Version: version, Serial: 7, Row: row, Col: 1, Cols: 8, Rows: 3}}
}

func screen(typed string) []string {
	return []string{"top", "a", typed, "c", "bottom"}
}

func TestAnUnchangedRasterOnARepaintedRowIsCopiedNotEncoded(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsSixel)
	img := gradient(40, 30)

	_, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls)
	first := append([]byte(nil), probe.last...)
	require.NotEmpty(t, first)

	out.Reset()
	// A keystroke repaints row 3, which the picture covers: the picture must
	// be sent again, and it is the same picture in the same place.
	stats, err := surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Equal(t, 1, stats.PlacementsSent, "text walked over the picture: it must be sent again")
	require.Equal(t, 1, probe.calls, "the resend is a copy of the first encoding, not a new one")
	require.True(t, bytes.Contains(out.Bytes(), first), "the resent bytes are exactly the first encoding")
}

// The control for the test above: with the cache off the same resend goes
// through the encoder, so the counter is measuring what it claims to.
func TestWithoutTheCacheTheSameResendEncodesAgain(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsSixel)
	surface.encodedLimit = 0
	img := gradient(40, 30)

	_, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	_, err = surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Equal(t, 2, probe.calls)
	require.Empty(t, surface.encoded, "a disabled cache keeps nothing")
}

func TestAChangedRasterIsEncodedAgain(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsSixel)
	img := gradient(40, 30)

	_, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	// New pixels, same place: the cached bytes describe the old picture.
	stats, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 2, 2)})
	require.NoError(t, err)
	require.Equal(t, 1, stats.PlacementsSent)
	require.Equal(t, 2, probe.calls, "a new version must be encoded, not copied")

	// A moved picture is the same payload in a different frame: the cache
	// holds the payload, and the position is added when it is sent.
	_, err = surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 2, 3)})
	require.NoError(t, err)
	require.Equal(t, 2, probe.calls, "a moved sixel picture is framed again, not encoded")
}

// Screen-origin sixel starts a picture on a six-row band, so its payload
// depends on (row-1)*cellH % 6 and on nothing else about the position.
// With 10×20 cells rows 1 and 4 share a class (0 and 60) and row 2 does not
// (20 % 6 = 2).
func TestAMovedSixelPictureIsEncodedOncePerBandClass(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsSixel)
	cells := ttyapi.NewProbe(func(string) string { return "" })
	cells.SetCellSize(10, 20)
	surface.probe = cells
	// Few enough colours for the exact-palette encoder, whose output is
	// deterministic; the general one orders its palette by map iteration.
	img := fullPalette()
	rows := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	at := func(row, col int) ttyapi.Placement {
		return ttyapi.Placement{ID: "icon", Image: img, Version: 1, Serial: 1, Row: row, Col: col, Cols: 4, Rows: 2}
	}

	_, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{at(1, 1)}})
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls)

	out.Reset()
	_, err = surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{at(4, 7)}})
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls, "same band class: the payload is reused")
	fresh := appendSixel(nil, at(4, 7), cells)
	require.NotEmpty(t, fresh)
	require.True(t, bytes.Contains(out.Bytes(), fresh), "the reused payload frames to exactly a fresh encoding")

	out.Reset()
	_, err = surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{at(2, 7)}})
	require.NoError(t, err)
	require.Equal(t, 2, probe.calls, "another band class needs its own payload")
	require.True(t, bytes.Contains(out.Bytes(), appendSixel(nil, at(2, 7), cells)))
}

func TestARemovedPlacementLeavesTheCache(t *testing.T) {
	var out bytes.Buffer
	surface, _ := probedSurface(&out, graphicsSixel)
	img := gradient(40, 30)

	_, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Len(t, surface.encoded, 1)
	require.Positive(t, surface.encodedBytes)

	_, err = surface.Present(ttyapi.Frame{Rows: screen("x")})
	require.NoError(t, err)
	require.Empty(t, surface.encoded, "a placement gone from the frame is gone from the cache")
	require.Zero(t, surface.encodedBytes)
}

func TestKittyPutsAKnownImageByIDInsteadOfTransmittingIt(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsKitty)
	img := gradient(40, 30)
	id := fmt.Sprintf("i=%d", graphicsID("client"))

	_, err := surface.Present(ttyapi.Frame{Rows: screen("x"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Contains(t, out.String(), "a=T")
	require.NotContains(t, out.String(), "p=", "no placement id: WezTerm doubles cells holding placements with one")
	require.Contains(t, out.String(), ",d=i,a=d", "the old placements of the image go first, the image stays")
	require.Equal(t, 1, probe.calls)

	out.Reset()
	stats, err := surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 1, 2)})
	require.NoError(t, err)
	require.Equal(t, 1, stats.PlacementsSent)
	require.Contains(t, out.String(), "a=p", "the row under it was repainted: put it again by id")
	require.Contains(t, out.String(), id)
	require.Less(t, strings.Index(out.String(), ",d=i,a=d"), strings.Index(out.String(), "a=p"),
		"the old placement is taken off before the put, so it replaces rather than adds")
	require.NotContains(t, out.String(), "a=T", "the terminal still holds the image")
	require.Equal(t, 1, probe.calls)

	out.Reset()
	// Moved: same image, another place — still a put, not a transmit.
	_, err = surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 1, 3)})
	require.NoError(t, err)
	require.Contains(t, out.String(), "a=p")
	require.NotContains(t, out.String(), "a=T")

	out.Reset()
	// New pixels: now the image itself has to travel.
	_, err = surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 2, 3)})
	require.NoError(t, err)
	require.Contains(t, out.String(), "a=T")
	require.Equal(t, 2, probe.calls)

	out.Reset()
	// After Invalidate the image is transmitted again — whatever reset the
	// screen may have taken the terminal's copy — but from the cache.
	surface.Invalidate()
	_, err = surface.Present(ttyapi.Frame{Rows: screen("typed"), Placements: client(img, 2, 3)})
	require.NoError(t, err)
	require.Contains(t, out.String(), "a=T")
	require.Equal(t, 2, probe.calls, "the same image at the same place is copied, not encoded")
}

func TestTheCacheStaysUnderItsLimit(t *testing.T) {
	var out bytes.Buffer
	surface, probe := probedSurface(&out, graphicsSixel)
	img := gradient(40, 30)
	rows := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	first := ttyapi.Placement{ID: "first", Image: img, Version: 1, Serial: 1, Row: 1, Col: 1, Cols: 8, Rows: 3}
	second := ttyapi.Placement{ID: "second", Image: img, Version: 1, Serial: 2, Row: 5, Col: 1, Cols: 8, Rows: 3}

	_, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{first}})
	require.NoError(t, err)
	size := len(probe.last)
	require.Positive(t, size)
	// Room for one encoding and a half: the second must push the first out.
	surface.encodedLimit = size + size/2

	_, err = surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{first, second}})
	require.NoError(t, err)
	require.LessOrEqual(t, surface.encodedBytes, surface.encodedLimit)
	require.Len(t, surface.encoded, 1)
	_, kept := surface.encoded["second"]
	require.True(t, kept, "the least recently written entry is the one evicted")
}

// 100 presents of a frame whose 500×300 raster sits on a row repainted every
// frame — a keystroke in an SDK window. "present" is the surface as it is now;
// "encode" is what every one of those presents cost before the cache and the
// kitty put: a full encoding per send.
//
//	go test -run '^$' -bench RasterOnARepaintedRow -benchtime=100x ./service/terminal/
func BenchmarkRasterOnARepaintedRow(b *testing.B) {
	img := gradient(500, 300)
	place := []ttyapi.Placement{{ID: "client", Image: img, Version: 1, Serial: 1, Row: 2, Col: 1, Cols: 50, Rows: 15}}
	for _, protocol := range []graphicsProtocol{graphicsSixel, graphicsKitty} {
		b.Run(string(protocol)+"/present", func(b *testing.B) {
			var out bytes.Buffer
			surface := NewSurface(&out, ttyapi.SurfaceOptions{})
			surface.graphics = protocol
			rows := make([]string, 20)
			if _, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: place}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows[5] = strconv.Itoa(i)
				out.Reset()
				if _, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: place}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(string(protocol)+"/encode", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = appendPlace(nil, protocol, place[0])
			}
		})
	}
}
