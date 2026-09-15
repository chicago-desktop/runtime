// SPDX-License-Identifier: MPL-2.0

package terminal

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/sixel"
	"github.com/stretchr/testify/require"
	ttyapi "github.com/wippyai/runtime/api/tty"
)

func testRaster(shade uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for index := 0; index < len(img.Pix); index += 4 {
		img.Pix[index] = shade
		img.Pix[index+3] = 0xff
	}
	return img
}

// graphicsSurface returns a surface that believes the terminal can show
// rasters. Detection reads the environment, and a test that depended on the
// environment would pass or fail by which terminal ran it.
func graphicsSurface(out *bytes.Buffer) *Surface {
	surface := NewSurface(out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsKitty
	return surface
}

func placement(id string, row int, version uint64, img image.Image) ttyapi.Placement {
	return ttyapi.Placement{
		ID: id, Image: img, Version: version,
		Row: row, Col: 1, Cols: 4, Rows: 3,
	}
}

func TestSurfacePlacesRaster(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0x10))},
	})
	require.NoError(t, err)

	painted := output.String()
	require.Contains(t, painted, "\x1b_G", "the graphics command must reach the terminal")
	require.Contains(t, painted, "a=T", "the raster is transmitted and placed in one command")
	// Raw pixels carry no header: without the transmitted size the terminal
	// has bytes it cannot interpret, and the failure is invisible — the
	// sequence looks correct and nothing appears.
	require.Contains(t, painted, "s=8,v=8", "the transmitted size must be stated")
	// Without this the terminal answers, the answer arrives on the input path
	// and is decoded as keystrokes.
	require.Contains(t, painted, "q=2", "the terminal's acknowledgement must be silenced")
}

func TestSurfaceKeepsUnchangedRasterOffTheWire(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)
	raster := testRaster(0x20)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three", "four"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)

	output.Reset()
	// The same raster in the same place, and the row it covers unchanged.
	// Resending it would be the difference between a still picture and a
	// flickering one.
	_, err = surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three", "four"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)
	require.NotContains(t, output.String(), "\x1b_G")
}

func TestSurfaceRepaintsRasterAfterTheRowUnderItChanges(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)
	raster := testRaster(0x30)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three", "four"},
		Placements: []ttyapi.Placement{placement("logo", 2, 1, raster)},
	})
	require.NoError(t, err)

	output.Reset()
	// Row three sits inside the placement's rectangle. Painting text there
	// destroyed that part of the picture, and nothing else would notice: for
	// the row itself the text is simply what it now says.
	_, err = surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "CHANGED", "four"},
		Placements: []ttyapi.Placement{placement("logo", 2, 1, raster)},
	})
	require.NoError(t, err)
	// Placed again — by id: the terminal still holds the image, only the
	// cells under the text lost it, so the full RGBA is not sent twice.
	require.Contains(t, output.String(), "a=p", "a raster walked over by text must be placed again")
	require.NotContains(t, output.String(), "a=T", "an unchanged image is not transmitted again")
}

func TestSurfaceLeavesRasterAloneWhenAnUncoveredRowChanges(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)
	raster := testRaster(0x40)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three", "four", "five"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)

	output.Reset()
	// Row five is below the placement, which covers rows one to three.
	_, err = surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three", "four", "CHANGED"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)
	require.NotContains(t, output.String(), "\x1b_G")
}

func TestSurfaceRemovesRasterMissingFromTheFrame(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0x50))},
	})
	require.NoError(t, err)

	output.Reset()
	// Placements are declarative and complete: one left out is taken off the
	// screen. Otherwise a raster outlives whatever put it there, and nobody
	// is left who remembers it exists.
	_, err = surface.Present(ttyapi.Frame{Rows: []string{"one"}})
	require.NoError(t, err)
	require.Contains(t, output.String(), "a=d", "a placement dropped from the frame must be deleted")
}

func TestSurfaceCloseTakesRastersOffTheScreen(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0x60))},
	})
	require.NoError(t, err)

	output.Reset()
	require.NoError(t, surface.Close())
	// A raster lives on the terminal's side and would survive the process
	// that drew it, left as litter on somebody else's screen.
	require.Contains(t, output.String(), "a=d")
}

func TestSurfaceSendsNothingWhenTheTerminalHasNoGraphics(t *testing.T) {
	var output bytes.Buffer
	surface := NewSurface(&output, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsNone

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0x70))},
	})
	require.NoError(t, err)

	// An escape sequence a terminal does not understand spills onto the
	// screen as text, which is worse than the missing picture: it reads as a
	// broken application.
	require.NotContains(t, output.String(), "\x1b_G")
	require.NotContains(t, output.String(), "_Gq")
}

func TestSurfaceIgnoresPlacementWithoutGeometry(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)

	_, err := surface.Present(ttyapi.Frame{
		Rows: []string{"one"},
		Placements: []ttyapi.Placement{
			{ID: "logo", Image: testRaster(0x80), Version: 1, Row: 1, Col: 1},
		},
	})
	require.NoError(t, err)
	require.NotContains(t, output.String(), "\x1b_G")
}

func TestDetectGraphicsNamesOnlyWhatItRecognises(t *testing.T) {
	for name, testCase := range map[string]struct {
		env  map[string]string
		want graphicsProtocol
	}{
		"kitty by window id": {env: map[string]string{"KITTY_WINDOW_ID": "1"}, want: graphicsKitty},
		"wezterm by program": {env: map[string]string{"TERM_PROGRAM": "WezTerm"}, want: graphicsKitty},
		"ghostty by term":    {env: map[string]string{"TERM": "xterm-ghostty"}, want: graphicsKitty},
		"plain xterm":        {env: map[string]string{"TERM": "xterm-256color"}, want: graphicsNone},
		"nothing at all":     {env: map[string]string{}, want: graphicsNone},
		"switched off on purpose": {env: map[string]string{
			"KITTY_WINDOW_ID": "1", "WIPPY_TTY_GRAPHICS": "off",
		}, want: graphicsNone},
	} {
		t.Run(name, func(t *testing.T) {
			protocol, _ := ttyapi.DetectGraphics(func(key string) string { return testCase.env[key] })
			got := graphicsProtocol(protocol)
			require.Equal(t, testCase.want, got)
		})
	}
}

func TestGraphicsIDIsStableAndNeverZero(t *testing.T) {
	// Zero means "no id" in the protocol: a placement that folded to zero
	// could never be deleted by name.
	require.NotZero(t, graphicsID(""))
	require.Equal(t, graphicsID("my_computer"), graphicsID("my_computer"))
	require.NotEqual(t, graphicsID("my_computer"), graphicsID("recycle_bin"))
}

func TestPlacementCoversOnlyItsOwnRows(t *testing.T) {
	state := placementState{row: 4, rows: 3}
	require.False(t, state.covers(3))
	require.True(t, state.covers(4))
	require.True(t, state.covers(6))
	require.False(t, state.covers(7))
}

func TestSurfacePlacementSurvivesInvalidate(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)
	raster := testRaster(0x90)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)

	surface.Invalidate()
	output.Reset()
	_, err = surface.Present(ttyapi.Frame{
		Rows:       []string{"one"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)
	// Invalidate means the surface no longer knows what is on screen. A
	// raster it does not resend would be gone from a repainted screen.
	require.True(t, strings.Contains(output.String(), "a=T"))
}

func sixelSurface(out *bytes.Buffer) *Surface {
	surface := NewSurface(out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel
	return surface
}

func TestSurfacePlacesRasterAsSixel(t *testing.T) {
	var output bytes.Buffer
	surface := sixelSurface(&output)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       []string{"one", "two", "three"},
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0xa0))},
	})
	require.NoError(t, err)

	painted := output.String()
	// Sixel travels in a device control string, not an application command:
	// sending the kitty form to a terminal that speaks sixel spills the
	// sequence onto the screen as text.
	require.Contains(t, painted, "\x1bP", "the sixel payload must be a device control string")
	require.Contains(t, painted, "\x1b\\", "the device control string must be terminated")
	require.NotContains(t, painted, "\x1b_G", "kitty commands must not be sent to a sixel terminal")
}

func TestSurfaceRepaintsCellsWhenASixelPictureLeaves(t *testing.T) {
	var output bytes.Buffer
	surface := sixelSurface(&output)
	rows := []string{"one", "two", "three", "four"}

	_, err := surface.Present(ttyapi.Frame{
		Rows:       rows,
		Placements: []ttyapi.Placement{placement("logo", 1, 1, testRaster(0xb0))},
	})
	require.NoError(t, err)

	output.Reset()
	// Sixel cannot remove a picture by name. The only way it goes away is the
	// cells being painted again — so the rows it covered must be treated as
	// stale even though their text did not change.
	_, err = surface.Present(ttyapi.Frame{Rows: rows})
	require.NoError(t, err)

	painted := output.String()
	require.Contains(t, painted, "one", "the covered rows must be painted again")
	require.Contains(t, painted, "three", "the whole rectangle must be painted again")
	require.NotContains(t, painted, "four", "rows the picture never covered stay untouched")
}

func TestSurfaceRepaintsCellsWhenASixelPictureMoves(t *testing.T) {
	var output bytes.Buffer
	surface := sixelSurface(&output)
	rows := []string{"one", "two", "three", "four", "five", "six"}
	raster := testRaster(0xc0)

	_, err := surface.Present(ttyapi.Frame{
		Rows:       rows,
		Placements: []ttyapi.Placement{placement("logo", 1, 1, raster)},
	})
	require.NoError(t, err)

	output.Reset()
	// A picture that moved vacates its old rectangle just as surely as one
	// that disappeared, and leaves a copy of itself behind if nobody paints
	// over it.
	_, err = surface.Present(ttyapi.Frame{
		Rows:       rows,
		Placements: []ttyapi.Placement{placement("logo", 4, 1, raster)},
	})
	require.NoError(t, err)
	require.Contains(t, output.String(), "one", "the rectangle it left must be painted again")
}

// The arrangement FR-005 is built on, pinned here so it cannot quietly stop
// being true: chrome cut into pieces by ROW, text underneath changing all the
// time, and only the pieces sharing rows with that text paid for.
//
// If this ever regresses, nothing breaks — the screen stays correct. It just
// gets slow, and slow has no stack trace.
func TestOnlyChromeSharingRowsWithTextIsResent(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel

	pixels := image.NewRGBA(image.Rect(0, 0, 10, 20))
	// A window at rows 3..9: title on its own rows, side borders sharing rows
	// with the content, bottom on its own row. Plus a taskbar far below.
	chrome := func(version uint64) []ttyapi.Placement {
		return []ttyapi.Placement{
			{ID: "title", Image: pixels, Version: version, Row: 3, Col: 5, Cols: 40, Rows: 2},
			{ID: "left", Image: pixels, Version: version, Row: 5, Col: 5, Cols: 1, Rows: 4},
			{ID: "right", Image: pixels, Version: version, Row: 5, Col: 44, Cols: 1, Rows: 4},
			{ID: "bottom", Image: pixels, Version: version, Row: 9, Col: 5, Cols: 40, Rows: 1},
			{ID: "taskbar", Image: pixels, Version: version, Row: 24, Col: 1, Cols: 80, Rows: 1},
		}
	}

	rows := make([]string, 24)
	for i := range rows {
		rows[i] = ""
	}
	rows[5] = "первый кадр"

	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: chrome(1)})
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if stats.PlacementsSent != 5 {
		t.Fatalf("the first frame must send everything, sent %d", stats.PlacementsSent)
	}

	// A keystroke: one content row changes, the rasters do not.
	changed := append([]string(nil), rows...)
	changed[5] = "первый кадр!"
	stats, err = surface.Present(ttyapi.Frame{Rows: changed, Placements: chrome(1)})
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if stats.PlacementsSent != 2 {
		t.Fatalf("a keystroke must cost the two side borders and nothing else, sent %d",
			stats.PlacementsSent)
	}

	// And a frame where nothing at all moved must cost nothing.
	stats, err = surface.Present(ttyapi.Frame{Rows: changed, Placements: chrome(1)})
	if err != nil {
		t.Fatalf("third frame: %v", err)
	}
	if stats.PlacementsSent != 0 {
		t.Fatalf("an unchanged frame must send nothing, sent %d", stats.PlacementsSent)
	}
	if stats.Bytes != 0 {
		t.Fatalf("an unchanged frame must write nothing, wrote %d bytes", stats.Bytes)
	}
}

// The control for the test above. Without it, "only two were sent" would also
// pass on a surface that had simply stopped sending anything.
func TestARasterThatChangedIsAlwaysResent(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel

	pixels := image.NewRGBA(image.Rect(0, 0, 10, 20))
	rows := make([]string, 10)
	place := func(version uint64) []ttyapi.Placement {
		return []ttyapi.Placement{
			{ID: "clock", Image: pixels, Version: version, Row: 9, Col: 70, Cols: 6, Rows: 1},
		}
	}

	if _, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: place(1)}); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: place(2)})
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if stats.PlacementsSent != 1 {
		t.Fatalf("a raster whose pixels changed must be resent, sent %d", stats.PlacementsSent)
	}
}

// A rebuilt raster is a different picture even when its version says
// otherwise — and the failure this prevents is the worst kind: not a slow
// screen, a WRONG one.
//
// A caller that builds a fresh raster every frame restarts the version count
// at one. Drawing it with the same number of calls lands on the same number
// again, so a clock showing 01:59 and the same clock showing 02:00 arrive
// carrying identical versions and identical geometry. Comparing versions
// alone, the surface takes the second for the first and leaves 01:59 on the
// screen. Nothing reports a fault; the clock has simply stopped.
func TestARebuiltRasterIsNotTheSamePicture(t *testing.T) {
	var out bytes.Buffer
	surface := NewSurface(&out, ttyapi.SurfaceOptions{})
	surface.graphics = graphicsSixel

	before := image.NewRGBA(image.Rect(0, 0, 10, 20))
	after := image.NewRGBA(image.Rect(0, 0, 10, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 10; x++ {
			after.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	rows := make([]string, 5)
	clock := func(img image.Image, serial uint64) []ttyapi.Placement {
		// Same version, same place, same size — only the buffer is new.
		return []ttyapi.Placement{
			{ID: "clock", Image: img, Version: 3, Serial: serial, Row: 5, Col: 70, Cols: 6, Rows: 1},
		}
	}

	if _, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: clock(before, 1)}); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: clock(after, 2)})
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if stats.PlacementsSent != 1 {
		t.Fatal("a rebuilt raster must be resent, or the screen keeps the stale picture")
	}

	// The control: the SAME buffer, unchanged, must still cost nothing.
	// Without this, "always resend" would pass the test above and quietly
	// undo the whole economy.
	stats, err = surface.Present(ttyapi.Frame{Rows: rows, Placements: clock(after, 2)})
	if err != nil {
		t.Fatalf("third frame: %v", err)
	}
	if stats.PlacementsSent != 0 {
		t.Fatalf("the same buffer unchanged must not be resent, sent %d", stats.PlacementsSent)
	}
}

// Closing must not speak kitty to a terminal that speaks sixel.
//
// The frame path already refuses this — an APC command a terminal does not
// understand is the failure it exists to avoid — and the close path did it
// anyway. A rule that holds in one of two places is a rule on its way out,
// and this one leaves its evidence on the screen after the program is gone.
func TestClosingSpeaksOnlyTheProtocolTheTerminalKnows(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		protocol graphicsProtocol
		wantAPC  bool
	}{
		{"sixel says nothing", graphicsSixel, false},
		{"kitty is told to forget", graphicsKitty, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var out bytes.Buffer
			surface := NewSurface(&out, ttyapi.SurfaceOptions{})
			surface.graphics = testCase.protocol

			pixels := image.NewRGBA(image.Rect(0, 0, 10, 20))
			_, err := surface.Present(ttyapi.Frame{
				Rows: make([]string, 4),
				Placements: []ttyapi.Placement{
					{ID: "x", Image: pixels, Version: 1, Serial: 1, Row: 1, Col: 1, Cols: 1, Rows: 1},
				},
			})
			require.NoError(t, err)

			out.Reset()
			require.NoError(t, surface.Close())

			gotAPC := bytes.Contains(out.Bytes(), []byte("\x1b_G"))
			if gotAPC != testCase.wantAPC {
				t.Fatalf("kitty command on close: got %v, want %v (%q)",
					gotAPC, testCase.wantAPC, out.String())
			}
		})
	}
}

// A picture on the last terminal row must not advance the terminal cursor.
// Save/restore cannot undo scrolling: every redraw would move the taskbar up
// while its mouse hit remains on the last row.
func TestTaskbarPlacementDoesNotScrollTheTerminal(t *testing.T) {
	var output bytes.Buffer
	surface := graphicsSurface(&output)
	rows := make([]string, 24)
	bar := ttyapi.Placement{
		ID: "bars", Row: 24, Col: 1, Cols: 80, Rows: 1,
		Image: testRaster(0x80), Serial: 1, Version: 1,
	}
	for version := uint64(1); version <= 3; version++ {
		output.Reset()
		bar.Version = version
		stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{bar}})
		require.NoError(t, err)
		require.Equal(t, 1, stats.PlacementsSent)
		stream := output.String()
		require.Contains(t, stream, "\x1b[24;1H", "the visible bar belongs to its mouse row")
		start := strings.Index(stream, "\x1b_G")
		require.NotEqual(t, -1, start)
		end := strings.IndexByte(stream[start:], ';')
		require.Positive(t, end)
		controls := strings.Split(stream[start+3:start+end], ",")
		require.Contains(t, controls, "C=1", "placing the bottom bar must never scroll the desktop")
	}
	output.Reset()
	stats, err := surface.Present(ttyapi.Frame{Rows: rows, Placements: []ttyapi.Placement{bar}})
	require.NoError(t, err)
	require.Zero(t, stats.Bytes, "an unchanged frame still sends nothing")
}

// Sixel's final six-pixel band can extend past the last cell even when the
// logical image fits. DECSDM clamps graphics instead of scrolling text.
func TestSixelBottomBarUsesNonScrollingScreenCoordinates(t *testing.T) {
	for _, cell := range []struct{ w, h int }{{10, 20}, {12, 23}, {8, 16}} {
		ttyapi.SetProbedCellSize(cell.w, cell.h)
		t.Cleanup(ttyapi.ForgetProbedGraphics)
		var output bytes.Buffer
		surface := sixelSurface(&output)
		imageWidth, imageHeight := 4*cell.w, 2*cell.h
		raster := image.NewRGBA(image.Rect(0, 0, imageWidth, imageHeight))
		for i := range raster.Pix {
			raster.Pix[i] = 0xff
		}
		bar := ttyapi.Placement{ID: "bars", Image: raster, Row: 23, Col: 3, Cols: 4, Rows: 2, Serial: 1}
		for version := uint64(1); version <= 3; version++ {
			output.Reset()
			bar.Version = version
			_, err := surface.Present(ttyapi.Frame{Rows: make([]string, 24), Placements: []ttyapi.Placement{bar}})
			require.NoError(t, err)
			stream := output.String()
			mode := strings.Index(stream, "\x1b[?80h")
			start := strings.Index(stream, "\x1bP")
			require.GreaterOrEqual(t, mode, 0, "sixel must not scroll a full-screen surface")
			require.Greater(t, start, mode)
			end := strings.Index(stream[start:], "\x1b\\") + start
			require.Greater(t, end, start)
			require.Contains(t, stream[end:], "\x1b[?80l", "restore ordinary sixel mode after the image")
			payload := stream[start+strings.IndexByte(stream[start:], 'q')+1 : end]
			decoded, err := (&sixel.Decoder{}).Decode(strings.NewReader(payload))
			require.NoError(t, err)
			x, y := (bar.Col-1)*cell.w, (bar.Row-1)*cell.h
			_, _, _, alpha := decoded.At(x, y).RGBA()
			require.EqualValues(t, 0xffff, alpha, "the bar starts on its mouse cell")
			_, _, _, above := decoded.At(x, y-1).RGBA()
			require.Zero(t, above, "positioning must not paint over windows above the bar")
			_, _, _, left := decoded.At(x-1, y).RGBA()
			require.Zero(t, left, "horizontal positioning stays transparent")
			_, _, _, last := decoded.At(x+imageWidth-1, y+imageHeight-1).RGBA()
			require.EqualValues(t, 0xffff, last, "no bottom pixels may be cropped to avoid scrolling")
		}
	}
}
